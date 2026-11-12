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
	promo        string       // canonical promo code, "" for none
	idemKey      string       // generated on first submit, reused for every retry of this order
	orderID      string       // set after a successful submit
	pendingInput *order.Input // snapshot of the submission when outcome is unresolved
}

// hasPending reports whether a submission has been attempted but the outcome
// is still unknown (a timeout, an unreadable reply, or a 5xx that may have
// followed creation upstream). While pending, edits are blocked so that a
// retry with the same idempotency key sends exactly the same contents.
func (s *session) hasPending() bool {
	return s.idemKey != "" && s.orderID == "" && s.pendingInput != nil
}

// blockedByPending prints why editing is paused and reports true when the
// caller should stop. Edits cannot be applied while a submission's outcome
// is unknown: the saved snapshot must keep matching its idempotency key.
func (s *session) blockedByPending() bool {
	if !s.hasPending() {
		return false
	}
	fmt.Fprintln(s.out, "That change was not applied: the previous submission's outcome is still unknown.")
	fmt.Fprintln(s.out, "Run 'submit' to retry it safely with the same idempotency key. Editing resumes once the outcome is known.")
	return true
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
		if len(args) > 0 {
			fmt.Fprintln(s.out, "Usage: describe (no arguments)")
			break
		}
		s.describe()
	case "tip":
		s.setTip(args)
	case "promo":
		s.setPromo(args)
	case "submit":
		// Arity is checked before anything is sent: `submit unexpected`
		// must not create an order.
		if len(args) > 0 {
			fmt.Fprintln(s.out, "Usage: submit (no arguments). Nothing was sent.")
			break
		}
		s.submit()
	case "status":
		if len(args) > 1 {
			fmt.Fprintln(s.out, "Usage: status [<order-id>]")
			break
		}
		s.status(args)
	case "update":
		if len(args) > 0 {
			fmt.Fprintln(s.out, "Usage: update (no arguments)")
			break
		}
		s.update()
	case "cancel":
		if len(args) > 1 {
			fmt.Fprintln(s.out, "Usage: cancel [<order-id>]")
			break
		}
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
	if s.blockedByPending() {
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
	if s.blockedByPending() {
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
	if s.blockedByPending() {
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
	if s.blockedByPending() {
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
	if s.blockedByPending() {
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
	if s.blockedByPending() {
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
		if strings.TrimLeft(frac, "0123456789") != "" {
			return 0, fmt.Errorf("%q has invalid digits in the fractional part", s)
		}
		cents, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a valid amount", s)
		}
	}
	// Bound the dollar component *before* multiplying, so the conversion
	// itself cannot wrap. MaxCents/100 is the largest whole-dollar value
	// that can still fit once converted.
	maxDollars := int64(pricing.MaxCents) / 100
	if dollars > maxDollars {
		return 0, fmt.Errorf("%q exceeds the maximum amount of %s", s, pricing.MaxCents)
	}
	total := dollars*100 + cents
	if total > int64(pricing.MaxCents) {
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

// validateOrder reports whether a 2xx reply actually describes an order.
// An empty body, `{}`, or JSON `null` decodes without error but carries no
// order: announcing success from such a reply would print blank fields and
// could overwrite a known order ID with "". Callers must treat a failure
// here as an unresolved outcome, not as a rejection.
func (r *orderResponse) validateOrder() error {
	if r == nil {
		return fmt.Errorf("the service returned no order data")
	}
	var missing []string
	if r.ID == "" {
		missing = append(missing, "id")
	}
	if r.Status == "" {
		missing = append(missing, "status")
	}
	if r.TotalFormatted == "" {
		missing = append(missing, "total_formatted")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the service's reply is missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// errorMessage returns the service's error text, falling back to the HTTP
// status so a non-JSON or empty error body still says something useful.
func (r *orderResponse) errorMessage(status int) string {
	if r != nil && r.Error != "" {
		return r.Error
	}
	return fmt.Sprintf("HTTP %d %s (no error message in the reply)", status, http.StatusText(status))
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
		// empty body, malformed JSON. Preserve timeout classification
		// (via %w) so callers can retry, and keep the HTTP status in the
		// message: "HTTP 201 but unreadable body" is a very different
		// situation from "could not reach the service at all".
		return resp.StatusCode, nil, fmt.Errorf("reading the HTTP %d response body: %w", resp.StatusCode, err)
	}
	return resp.StatusCode, &out, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ambiguousRequestFailure reports whether a 4xx status describes a failure
// to *deliver or read the request* rather than a decision to refuse it.
// These say nothing about whether the order was stored:
//
//   - 408 Request Timeout: the request did not arrive in time, but a proxy
//     can emit this after the origin already accepted and stored the order.
//   - 425 Too Early: the request was replayed too soon and may be retried;
//     it is explicitly not a judgement about the request's content.
//
// Callers must treat these as unresolved and keep the idempotency key.
func ambiguousRequestFailure(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly
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
// per local order and reused for every attempt at that order, so a
// submission whose outcome is uncertain can always be retried without
// risking a duplicate: the service returns the original order for a key it
// has already seen.
//
// Key/snapshot lifetime is the heart of this function. The key is cleared
// only when non-creation is actually *established*: a service validation
// rejection — a 4xx in which the service judged the request's contents and
// refused them, which it does before storing anything. That is narrower
// than "any 4xx": it excludes 409 (the key demonstrably *is* used) and the
// delivery failures 408/425, which report that the request did not arrive
// usably and say nothing about whether the order was stored.
//
// Everything else is ambiguous — timeouts, body-read errors, connection
// resets, 5xx from an intermediary, and any 2xx other than 200/201 — so
// the key and the submitted snapshot are kept and `submit` can be run
// again safely.
func (s *session) submit() {
	// If an earlier attempt left an unresolved outcome, resend exactly the
	// same contents under the same key. Rebuilding the body from the
	// current (possibly edited) state would make one key mean two
	// different orders.
	if s.hasPending() {
		fmt.Fprintln(s.out, "Retrying the previous submission — its outcome is still unknown.")
		fmt.Fprintln(s.out, "The original contents and idempotency key are reused, so this cannot create a duplicate.")
	} else {
		if s.orderID != "" {
			fmt.Fprintf(s.out, "Order %s is already submitted — use 'update' to change it, or 'cancel' to call it off.\n", s.orderID)
			return
		}
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
		// Transport or body-read failure. We cannot tell whether the
		// order was created, so the key and snapshot are preserved and
		// the outcome stays pending.
		fmt.Fprintf(s.out, "The submission did not complete: %v\n", err)
		s.reportUnresolved()
		return
	}

	// A 409 means the key is already bound to a different order — proof
	// that the key *was* used, so it must not be released. The service's
	// message names the original order ID.
	if status == http.StatusConflict {
		fmt.Fprintf(s.out, "The service could not accept this submission: %s\n", resp.errorMessage(status))
		fmt.Fprintln(s.out, "That idempotency key already belongs to an order, so it has been kept rather than reused for anything else.")
		fmt.Fprintln(s.out, "Use 'status <order-id>' with the ID above to see that order.")
		return
	}
	// Not every 4xx proves non-creation. A 408 (or 425) reports that the
	// *request* timed out or arrived too early, which says nothing about
	// whether the service had already stored the order — a proxy can emit
	// 408 after the origin accepted it. Treating these as definite
	// rejections and releasing the key is exactly how a retry creates a
	// duplicate, so they are classified as ambiguous instead.
	if ambiguousRequestFailure(status) {
		fmt.Fprintf(s.out, "The service returned HTTP %d: %s\n", status, resp.errorMessage(status))
		s.reportUnresolved()
		return
	}
	// What remains is a service *validation* rejection: the service read
	// the request, judged its contents, and refused it before storing
	// anything. That is the one case where non-creation is established,
	// so the key can be released and editing resumed. This is narrower
	// than "any 4xx": the delivery failures (408/425) and the key
	// conflict (409) are handled above precisely because they are not
	// judgements about the contents.
	//
	// The honest residual limit: a 4xx synthesized by an intermediary
	// that is neither 408 nor 425 (say a proxy's own 403 or 404) is
	// indistinguishable on the wire from the service's own rejection, so
	// it is treated as one. Narrowing further would need a signal the
	// service controls — an error envelope the builder can authenticate —
	// which is out of scope here and noted in the README.
	if status >= 400 && status < 500 {
		fmt.Fprintf(s.out, "The service rejected the order: %s\n", resp.errorMessage(status))
		fmt.Fprintln(s.out, "No order was created, so you can fix the problem and submit again.")
		s.idemKey = ""
		s.pendingInput = nil
		return
	}
	// Any other non-2xx (notably 5xx, possibly from an intermediary) is
	// ambiguous: upstream may have created the order before the failure.
	if status >= 300 {
		fmt.Fprintf(s.out, "The service returned an error: %s\n", resp.errorMessage(status))
		s.reportUnresolved()
		return
	}
	// Only 200 (idempotent replay) and 201 (created) are defined outcomes
	// for a submission. Any other 2xx — 202 Accepted, 204 No Content, a
	// 206 from a confused intermediary — does not establish that the order
	// exists in a final state, so it must not be announced as completion.
	// 202 in particular means "queued, not yet processed".
	if status != http.StatusOK && status != http.StatusCreated {
		fmt.Fprintf(s.out, "The service replied HTTP %d, which this client does not treat as a completed submission.\n", status)
		if status == http.StatusAccepted {
			fmt.Fprintln(s.out, "HTTP 202 means the request was accepted for processing but not necessarily stored yet.")
		}
		s.reportUnresolved()
		return
	}
	// 2xx, but the body must actually describe an order before we claim
	// success or overwrite session state.
	if err := resp.validateOrder(); err != nil {
		fmt.Fprintf(s.out, "The service replied HTTP %d but %v.\n", status, err)
		s.reportUnresolved()
		return
	}

	// Resolved: the order exists and we know its ID.
	s.orderID = resp.ID
	s.pendingInput = nil
	if status == http.StatusOK {
		fmt.Fprintln(s.out, "The service already had this order (idempotent replay) — no duplicate was created.")
	}
	fmt.Fprintf(s.out, "Order %s submitted. Status: %s. Total charged: %s.\n", resp.ID, resp.Status, resp.TotalFormatted)
	fmt.Fprintln(s.out, "Changes you make locally from here are NOT sent until you run 'update' (and only while the order is Not Started).")
}

// reportUnresolved explains an ambiguous submission outcome and the
// recovery path. The key and snapshot are deliberately left in place: the
// order may already exist, so retrying under the same key is the only safe
// action.
func (s *session) reportUnresolved() {
	fmt.Fprintln(s.out, "The order may or may not have been created, so nothing has been changed locally.")
	fmt.Fprintln(s.out, "Run 'submit' again to retry safely with the same idempotency key — if the order already exists you will get that original order back.")
	fmt.Fprintln(s.out, "Editing is paused until the outcome is known. ('status <order-id>' can look up an order you already know the ID of, but it does not resolve this submission.)")
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
		fmt.Fprintf(s.out, "Could not read the order's status: %v\n", err)
		return
	}
	if status >= 300 {
		fmt.Fprintf(s.out, "The service said: %s\n", resp.errorMessage(status))
		return
	}
	if err := resp.validateOrder(); err != nil {
		fmt.Fprintf(s.out, "The service replied HTTP %d but %v.\n", status, err)
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
		// An update is a wholesale replacement of items, tip, and promo,
		// so it is naturally idempotent: applying the same body twice
		// leaves the same order. Retrying is therefore safe and needs no
		// idempotency key — unlike submit, a repeat cannot duplicate
		// anything. The only thing unknown is whether it landed yet.
		fmt.Fprintf(s.out, "The update did not complete: %v\n", err)
		fmt.Fprintln(s.out, "It may or may not have been applied. Running 'update' again is safe — it replaces the whole order, so applying it twice is the same as once.")
		s.explainStatusIsNotAContentsCheck()
		return
	}
	if status >= 300 {
		fmt.Fprintf(s.out, "The service rejected the update: %s\n", resp.errorMessage(status))
		if status == http.StatusConflict {
			// The kitchen has moved on; no retry will help.
			fmt.Fprintln(s.out, "The order can no longer be changed, so retrying will not help. Run 'status' to see where it is.")
		} else if status >= 500 || ambiguousRequestFailure(status) {
			fmt.Fprintln(s.out, "Running 'update' again is safe if you want to retry.")
		}
		return
	}
	if err := resp.validateOrder(); err != nil {
		fmt.Fprintf(s.out, "The service replied HTTP %d but %v.\n", status, err)
		fmt.Fprintln(s.out, "The update may still have been applied. Running 'update' again is safe — it replaces the whole order.")
		s.explainStatusIsNotAContentsCheck()
		return
	}
	fmt.Fprintf(s.out, "Order %s updated. New total: %s.\n", resp.ID, resp.TotalFormatted)
}

// explainStatusIsNotAContentsCheck states exactly what `status` can and
// cannot settle after an uncertain update. `status` prints the order's ID,
// lifecycle status, and charged total only — it does not list items,
// toppings, tip, or promo — so it cannot confirm *which* contents the
// service holds. Two different orders can share a total, and the surest
// recovery is to resend the same update rather than to infer from a figure.
func (s *session) explainStatusIsNotAContentsCheck() {
	fmt.Fprintln(s.out, "'status' reports only the order's lifecycle status and charged total, not its items, tip, or promo,")
	fmt.Fprintln(s.out, "so a matching total is not proof the update landed. To read the stored contents directly:")
	fmt.Fprintf(s.out, "  GET %s/orders/%s\n", s.client.base, s.orderID)
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
		fmt.Fprintf(s.out, "The cancellation did not complete: %v\n", err)
		fmt.Fprintf(s.out, "It may or may not have taken effect — run 'status %s' to check before retrying.\n", id)
		return
	}
	if status >= 300 {
		fmt.Fprintf(s.out, "The service rejected the cancellation: %s\n", resp.errorMessage(status))
		return
	}
	if err := resp.validateOrder(); err != nil {
		fmt.Fprintf(s.out, "The service replied HTTP %d but %v.\n", status, err)
		fmt.Fprintf(s.out, "Run 'status %s' to confirm whether it was cancelled.\n", id)
		return
	}
	fmt.Fprintf(s.out, "Order %s cancelled.\n", resp.ID)
}
