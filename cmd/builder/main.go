// The order builder: an interactive command-line program a customer uses
// to assemble an order and send it to the order service.
//
// The builder prices the order locally with the same pricing package the
// service uses, and shows the running total after every change. On submit
// it sends only the order's contents (items, tip, promo) — never a total —
// and the service computes the authoritative total with the same code, so
// the two cannot disagree.
//
// Usage:
//
//	go run ./cmd/builder [-service http://localhost:8080] [-timeout 5s]
//
// Type "help" at the prompt for the command list. The -timeout flag is the
// per-request HTTP timeout; submissions that time out are retried once
// with the same idempotency key (see submit).
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"pizzabuilder/internal/order"
	"pizzabuilder/internal/pricing"
)

func main() {
	service := flag.String("service", "http://localhost:8080", "base URL of the order service")
	timeout := flag.Duration("timeout", 5*time.Second, "per-request timeout for calls to the order service")
	flag.Parse()

	s := &session{
		client: &apiClient{base: strings.TrimRight(*service, "/"), hc: &http.Client{Timeout: *timeout}},
		out:    os.Stdout,
	}
	fmt.Fprintf(s.out, "Pizza order builder — talking to the order service at %s.\n", *service)
	fmt.Fprintln(s.out, "Type 'help' for commands, 'menu' to see the menu.")
	s.showTotals()

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for {
		fmt.Fprint(s.out, "pizza> ")
		if !sc.Scan() {
			fmt.Fprintln(s.out)
			return // EOF (or input error): exit quietly
		}
		if s.dispatch(strings.TrimSpace(sc.Text())) {
			return // quit
		}
	}
}

// ---------------------------------------------------------------------------
// Session state
// ---------------------------------------------------------------------------

type session struct {
	client       *apiClient
	out          io.Writer
	items        []pricing.LineItem
	tip          pricing.Tip
	promo        string // canonical promo code, "" for none
	idemKey      string // generated on first submit, reused for every retry of this order
	orderID      string // set after a successful submit
	pendingInput *order.Input // snapshot of the submission when outcome is unresolved
}

// hasPending reports whether a submission has been attempted but the outcome
// is still unknown (e.g. a timeout). While pending, edits are blocked so
// that a retry with the same idempotency key sends the same contents.
func (s *session) hasPending() bool {
	return s.idemKey != "" && s.orderID == "" && s.pendingInput != nil
}

func (s *session) input() order.Input {
	return order.Input{Items: s.items, Tip: s.tip, PromoCode: s.promo}
}

// dispatch runs one command line and reports whether the program should exit.
func (s *session) dispatch(line string) bool {
	if line == "" {
		return false
	}
	fields := strings.Fields(line)
	args := fields[1:]
	switch fields[0] {
	case "quit", "exit":
		return true
	case "help":
		s.help()
	case "menu":
		s.menu()
	case "add":
		s.add(args)
	case "remove":
		s.remove(args)
	case "setqty":
		s.setqty(args)
	case "settoppings":
		s.settoppings(args)
	case "describe":
		s.describe()
	case "tip":
		s.setTip(args)
	case "promo":
		s.setPromo(args)
	case "submit":
		s.submit()
	case "status":
		s.status(args)
	case "update":
		s.update()
	case "cancel":
		s.cancel(args)
	default:
		fmt.Fprintf(s.out, "Unknown command %q — type 'help' for the list.\n", fields[0])
	}
	return false
}

func (s *session) help() {
	fmt.Fprint(s.out, `Commands:
  menu                                  show sizes, toppings, and promos
  add <size> <qty> [topping,topping]    add a line item, e.g. add large 2 bacon,pineapple
  remove <line#>                        remove a line item (see 'describe' for line numbers)
  setqty <line#> <qty>                  change a line's quantity
  settoppings <line#> <list|none>       replace a line's toppings
  describe                              show the order and its running total
  tip amount <dollars>                  e.g. tip amount 3.50
  tip percent <0-100>                   e.g. tip percent 20
  tip clear                             remove the tip
  promo <CODE>                          apply a promo code (ILOVEPIZZA, SAVE5)
  promo clear                           remove the promo code
  submit                                send the order to the restaurant
  status [order-id]                     check a submitted order's status
  update                                send local changes to a submitted order (Not Started only)
  cancel [order-id]                     cancel a submitted order (Not Started only)
  quit                                  exit
Topping names may use underscores instead of spaces: extra_cheese.
`)
}

func (s *session) menu() {
	var sizes []string
	for _, sz := range pricing.Sizes() {
		sizes = append(sizes, fmt.Sprintf("%s %s", sz.Name, sz.BasePrice))
	}
	fmt.Fprintf(s.out, "Sizes: %s\n", strings.Join(sizes, " · "))
	for _, cat := range pricing.Categories() {
		var names, prices []string
		for _, t := range pricing.Toppings() {
			if t.Category == cat.Name {
				names = append(names, t.Name)
			}
		}
		for _, sz := range pricing.Sizes() {
			// Every topping in a category costs the same on a given size.
			prices = append(prices, pricing.ToppingPrice(sz, pricing.Topping{Category: cat.Name}).String())
		}
		fmt.Fprintf(s.out, "%s toppings (%d%% of base; %s by size): %s\n",
			cat.Name, cat.PercentOfBase, strings.Join(prices, "/"), strings.Join(names, ", "))
	}
	for _, p := range pricing.Promos() {
		fmt.Fprintf(s.out, "Promo %s: %s\n", p.Code, p.Description)
	}
}

// ---------------------------------------------------------------------------
// Local order editing
// ---------------------------------------------------------------------------

func (s *session) add(args []string) {
	if s.hasPending() {
		fmt.Fprintln(s.out, "The previous submission's outcome is unknown (timed out). Use 'submit' to retry, or 'status <order-id>' if you know the order ID.")
		return
	}
	if len(args) < 2 || len(args) > 3 {
		fmt.Fprintln(s.out, "Usage: add <size> <qty> [topping,topping,...]")
		return
	}
	size, ok := pricing.LookupSize(args[0])
	if !ok {
		fmt.Fprintf(s.out, "Unknown size %q. Sizes: Small, Medium, Large.\n", args[0])
		return
	}
	qty, err := strconv.Atoi(args[1])
	if err != nil || qty < 1 {
		fmt.Fprintf(s.out, "Quantity must be a whole number of at least 1, got %q.\n", args[1])
		return
	}
	if qty > pricing.MaxQuantity {
		fmt.Fprintf(s.out, "Quantity %d exceeds the maximum of %d per line item.\n", qty, pricing.MaxQuantity)
		return
	}
	var toppings []string
	if len(args) == 3 {
		toppings, err = parseToppings(args[2])
		if err != nil {
			fmt.Fprintln(s.out, err)
			return
		}
	}
	s.items = append(s.items, pricing.LineItem{Size: size.Name, Quantity: qty, Toppings: toppings})
	fmt.Fprintf(s.out, "Added line %d.\n", len(s.items))
	s.showTotals()
}

func (s *session) remove(args []string) {
	if s.hasPending() {
		fmt.Fprintln(s.out, "The previous submission's outcome is unknown (timed out). Use 'submit' to retry, or 'status <order-id>' if you know the order ID.")
		return
	}
	idx, ok := s.lineIndex(args)
	if !ok {
		return
	}
	s.items = append(s.items[:idx], s.items[idx+1:]...)
	fmt.Fprintf(s.out, "Removed line %d.\n", idx+1)
	s.showTotals()
}

func (s *session) setqty(args []string) {
	if s.hasPending() {
		fmt.Fprintln(s.out, "The previous submission's outcome is unknown (timed out). Use 'submit' to retry, or 'status <order-id>' if you know the order ID.")
		return
	}
	if len(args) != 2 {
		fmt.Fprintln(s.out, "Usage: setqty <line#> <qty>")
		return
	}
	idx, ok := s.lineIndex(args[:1])
	if !ok {
		return
	}
	qty, err := strconv.Atoi(args[1])
	if err != nil || qty < 1 {
		fmt.Fprintf(s.out, "Quantity must be a whole number of at least 1, got %q. Use 'remove' to drop a line.\n", args[1])
		return
	}
	if qty > pricing.MaxQuantity {
		fmt.Fprintf(s.out, "Quantity %d exceeds the maximum of %d per line item.\n", qty, pricing.MaxQuantity)
		return
	}
	s.items[idx].Quantity = qty
	s.showTotals()
}

func (s *session) settoppings(args []string) {
	if s.hasPending() {
		fmt.Fprintln(s.out, "The previous submission's outcome is unknown (timed out). Use 'submit' to retry, or 'status <order-id>' if you know the order ID.")
		return
	}
	if len(args) != 2 {
		fmt.Fprintln(s.out, "Usage: settoppings <line#> <topping,topping|none>")
		return
	}
	idx, ok := s.lineIndex(args[:1])
	if !ok {
		return
	}
	if args[1] == "none" {
		s.items[idx].Toppings = nil
	} else {
		toppings, err := parseToppings(args[1])
		if err != nil {
			fmt.Fprintln(s.out, err)
			return
		}
		s.items[idx].Toppings = toppings
	}
	s.showTotals()
}

// lineIndex parses a 1-based line number argument into a 0-based index.
func (s *session) lineIndex(args []string) (int, bool) {
	if len(args) != 1 {
		fmt.Fprintln(s.out, "Expected a single line number (see 'describe').")
		return 0, false
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 || n > len(s.items) {
		fmt.Fprintf(s.out, "No line %q in the current order (it has %d line(s)).\n", args[0], len(s.items))
		return 0, false
	}
	return n - 1, true
}

// parseToppings converts a comma-separated list into canonical menu names.
func parseToppings(csv string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		t, ok := pricing.LookupTopping(part)
		if !ok {
			return nil, fmt.Errorf("unknown topping %q — type 'menu' to see the toppings", part)
		}
		out = append(out, t.Name)
	}
	return out, nil
}

func (s *session) setTip(args []string) {
	if s.hasPending() {
		fmt.Fprintln(s.out, "The previous submission's outcome is unknown (timed out). Use 'submit' to retry, or 'status <order-id>' if you know the order ID.")
		return
	}
	switch {
	case len(args) == 1 && args[0] == "clear":
		s.tip = pricing.Tip{}
	case len(args) == 2 && args[0] == "amount":
		amount, err := parseMoney(args[1])
		if err != nil {
			fmt.Fprintf(s.out, "Bad tip amount: %v. Example: tip amount 3.50\n", err)
			return
		}
		s.tip = pricing.Tip{Mode: pricing.TipAmount, Amount: amount}
	case len(args) == 2 && args[0] == "percent":
		pct, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || pct < 0 || pct > 100 {
			fmt.Fprintf(s.out, "Tip percentage must be a whole number between 0 and 100, got %q.\n", args[1])
			return
		}
		s.tip = pricing.Tip{Mode: pricing.TipPercent, Percent: pct}
	default:
		fmt.Fprintln(s.out, "Usage: tip amount <dollars> | tip percent <0-100> | tip clear")
		return
	}
	s.showTotals()
}

func (s *session) setPromo(args []string) {
	if s.hasPending() {
		fmt.Fprintln(s.out, "The previous submission's outcome is unknown (timed out). Use 'submit' to retry, or 'status <order-id>' if you know the order ID.")
		return
	}
	if len(args) != 1 {
		fmt.Fprintln(s.out, "Usage: promo <CODE> | promo clear")
		return
	}
	if strings.EqualFold(args[0], "clear") {
		s.promo = ""
		fmt.Fprintln(s.out, "Promo cleared.")
		s.showTotals()
		return
	}
	code, err := pricing.ApplyPromo(s.promo, args[0])
	if err != nil {
		fmt.Fprintln(s.out, err)
		return
	}
	// Trial-price the order with the new promo so a promo that does not
	// qualify (e.g. SAVE5 at $20 or less) is rejected now, with the reason,
	// instead of surprising the customer at submit time.
	if _, err := pricing.Price(s.items, s.tip, code); err != nil && len(s.items) > 0 {
		fmt.Fprintln(s.out, err)
		return
	}
	s.promo = code
	fmt.Fprintf(s.out, "Promo %s applied.\n", code)
	s.showTotals()
}

func (s *session) describe() {
	if len(s.items) == 0 {
		fmt.Fprintln(s.out, "The order is empty.")
		return
	}
	desc, err := pricing.Describe(s.items)
	if err != nil {
		fmt.Fprintln(s.out, err)
		return
	}
	fmt.Fprintln(s.out, desc)
	s.showTotals()
}

// showTotals prints the running price breakdown — the total the customer
// watches while building. It is computed by the pricing package, the same
// code the service charges with. If the order currently cannot be priced
// (empty, or a promo that stopped qualifying after an edit), it says why
// instead of showing a misleading total.
func (s *session) showTotals() {
	if len(s.items) == 0 {
		fmt.Fprintln(s.out, "Total: $0.00 (order is empty)")
		return
	}
	t, err := pricing.Price(s.items, s.tip, s.promo)
	if err != nil {
		fmt.Fprintf(s.out, "Total: — (%v)\n", err)
		return
	}
	fmt.Fprintf(s.out, "  Subtotal: %s\n", t.Subtotal)
	if t.Discount > 0 {
		fmt.Fprintf(s.out, "  Discount (%s): -%s\n", s.promo, t.Discount)
	}
	fmt.Fprintf(s.out, "  Tax (6.5%%): %s\n", t.Tax)
	if t.Tip > 0 {
		fmt.Fprintf(s.out, "  Tip: %s\n", t.Tip)
	}
	fmt.Fprintf(s.out, "  Total: %s\n", t.Total)
}

// parseMoney parses a dollar amount like "3.50", "3.5", "3", or "$3.50"
// into cents, rejecting anything that is not a non-negative amount with at
// most two decimal places.
func parseMoney(s string) (pricing.Cents, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("amount cannot be negative")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return 0, fmt.Errorf("%q is not a valid amount", s)
	}
	dollars, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid amount", s)
	}
	if dollars < 0 {
		return 0, fmt.Errorf("amount cannot be negative")
	}
	var cents int64
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) == 0 || len(frac) > 2 {
			return 0, fmt.Errorf("%q has more than two decimal places", s)
		}
		frac = (frac + "00")[:2]
		cents, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a valid amount", s)
		}
	}
	// Check for overflow: MaxCents is the maximum monetary value.
	total := dollars*100 + cents
	if total < 0 || total > int64(pricing.MaxCents) {
		return 0, fmt.Errorf("%q exceeds the maximum amount of %s", s, pricing.MaxCents)
	}
	return pricing.Cents(total), nil
}

// ---------------------------------------------------------------------------
// Talking to the order service
// ---------------------------------------------------------------------------

type apiClient struct {
	base string
	hc   *http.Client
}

// orderResponse mirrors the service's order JSON (plus its error shape).
type orderResponse struct {
	ID             string         `json:"id"`
	Status         string         `json:"status"`
	Totals         pricing.Totals `json:"totals"`
	TotalFormatted string         `json:"total_formatted"`
	Error          string         `json:"error"`
}

func (c *apiClient) call(method, path string, in *order.Input, idemKey string) (int, *orderResponse, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return 0, nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out orderResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// Body read or decode failure — e.g. timeout reading the body,
		// empty body, malformed JSON. Preserve timeout classification so
		// callers can retry.
		return resp.StatusCode, nil, fmt.Errorf("reading response body: %w", err)
	}
	return resp.StatusCode, &out, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func newIdemKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand is not expected to fail; fall back to a timestamp key
		// rather than submitting without idempotency protection.
		return fmt.Sprintf("builder-%d", time.Now().UnixNano())
	}
	return "builder-" + hex.EncodeToString(b[:])
}

// submit sends the order to the service. One idempotency key is generated
// per local order and reused across retries, so if a submission times out
// — the request may or may not have arrived — retrying with the same key
// returns the original order instead of creating a duplicate. The retry
// happens once, and the customer is told that it happened and why.
func (s *session) submit() {
	// If there's a pending submission, use the saved snapshot so the
	// idempotency key always maps to the same contents.
	if s.hasPending() {
		fmt.Fprintln(s.out, "Reusing the previous submission — the outcome was unknown after a timeout.")
	} else {
		if len(s.items) == 0 {
			fmt.Fprintln(s.out, "Nothing to submit — the order is empty.")
			return
		}
		if _, err := pricing.Price(s.items, s.tip, s.promo); err != nil {
			fmt.Fprintf(s.out, "Cannot submit yet: %v\n", err)
			return
		}
		if s.idemKey == "" {
			s.idemKey = newIdemKey()
		}
		in := s.input()
		s.pendingInput = &in
	}

	status, resp, err := s.client.call(http.MethodPost, "/orders", s.pendingInput, s.idemKey)
	if err != nil && isTimeout(err) {
		fmt.Fprintln(s.out, "The submission timed out; the service may or may not have received it.")
		fmt.Fprintln(s.out, "Retrying once with the same idempotency key — if the first attempt did arrive, you get the original order back, not a duplicate.")
		status, resp, err = s.client.call(http.MethodPost, "/orders", s.pendingInput, s.idemKey)
	}
	if err != nil {
		fmt.Fprintf(s.out, "Could not reach the order service: %v\n", err)
		if !isTimeout(err) {
			// Non-timeout error: the request didn't reach the server,
			// so the idempotency key was never consumed. Reset so the
			// user can edit and try again fresh.
			s.idemKey = ""
			s.pendingInput = nil
		}
		// On timeout: keep the pending state so the user can retry.
		return
	}
	// Submission reached the server.
	s.pendingInput = nil
	if status >= 300 {
		fmt.Fprintf(s.out, "The service rejected the order: %s\n", resp.Error)
		// A rejection means the order was NOT created, so the key is unused.
		s.idemKey = ""
		return
	}
	s.orderID = resp.ID
	if status == http.StatusOK {
		fmt.Fprintf(s.out, "The service already had this order (idempotent replay) — no duplicate was created.\n")
	}
	fmt.Fprintf(s.out, "Order %s submitted. Status: %s. Total charged: %s.\n", resp.ID, resp.Status, resp.TotalFormatted)
	fmt.Fprintln(s.out, "Changes you make locally from here are NOT sent until you run 'update' (and only while the order is Not Started).")
}

func (s *session) status(args []string) {
	id := s.orderID
	if len(args) == 1 {
		id = args[0]
	}
	if id == "" {
		fmt.Fprintln(s.out, "No order submitted yet — run 'submit' first, or give an order ID.")
		return
	}
	status, resp, err := s.client.call(http.MethodGet, "/orders/"+id, nil, "")
	if err != nil {
		fmt.Fprintf(s.out, "Could not reach the order service: %v\n", err)
		return
	}
	if status >= 300 {
		fmt.Fprintf(s.out, "The service said: %s\n", resp.Error)
		return
	}
	fmt.Fprintf(s.out, "Order %s — status: %s — total: %s\n", resp.ID, resp.Status, resp.TotalFormatted)
}

func (s *session) update() {
	if s.orderID == "" {
		fmt.Fprintln(s.out, "No order submitted yet — run 'submit' first.")
		return
	}
	if len(s.items) == 0 {
		fmt.Fprintln(s.out, "The local order is empty; cancel instead if you want to call the order off.")
		return
	}
	in := s.input()
	status, resp, err := s.client.call(http.MethodPatch, "/orders/"+s.orderID, &in, "")
	if err != nil {
		fmt.Fprintf(s.out, "Could not reach the order service: %v\n", err)
		return
	}
	if status >= 300 {
		fmt.Fprintf(s.out, "The service rejected the update: %s\n", resp.Error)
		return
	}
	fmt.Fprintf(s.out, "Order %s updated. New total: %s.\n", resp.ID, resp.TotalFormatted)
}

func (s *session) cancel(args []string) {
	id := s.orderID
	if len(args) == 1 {
		id = args[0]
	}
	if id == "" {
		fmt.Fprintln(s.out, "No order submitted yet — run 'submit' first, or give an order ID.")
		return
	}
	status, resp, err := s.client.call(http.MethodPost, "/orders/"+id+"/cancel", nil, "")
	if err != nil {
		fmt.Fprintf(s.out, "Could not reach the order service: %v\n", err)
		return
	}
	if status >= 300 {
		fmt.Fprintf(s.out, "The service rejected the cancellation: %s\n", resp.Error)
		return
	}
	fmt.Fprintf(s.out, "Order %s cancelled.\n", resp.ID)
}
