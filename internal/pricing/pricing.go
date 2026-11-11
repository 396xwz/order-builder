// Package pricing is the single source of truth for the menu and for order
// totals. Both programs in this repository — the order builder (cmd/builder)
// and the order service (cmd/orderservice) — import this package, so the
// total a customer watches while building an order is guaranteed, by
// construction, to be the total the restaurant charges: it is the same code
// computing it in both places, and the service recomputes it server-side on
// every submission and update rather than trusting any client-supplied total.
package pricing

import (
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Money
// ---------------------------------------------------------------------------

// Cents is an amount of money in whole cents. Money is never represented as
// a float anywhere in this system: floats cannot represent common decimal
// prices exactly (0.1 + 0.2 != 0.3), which is how billing bugs are born.
// Formatting to dollars happens only at display time, via String.
type Cents int64

// String formats the amount as dollars and whole cents, e.g. $12.34.
func (c Cents) String() string {
	v := int64(c)
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s$%d.%02d", sign, v/100, v%100)
}

// percentOf returns num/den of c, rounded half up to the nearest cent.
// Every amount in this system is non-negative (discounts are floored at the
// subtotal), so adding den/2 before integer division implements
// round-half-up exactly, with no floating point involved. This is the ONLY
// rounding rule in the system, and it is applied in exactly three places:
// percentage promo discounts, tax, and percentage tips.
func percentOf(c Cents, num, den int64) Cents {
	return Cents((int64(c)*num + den/2) / den)
}

// ---------------------------------------------------------------------------
// Menu data
//
// The menu is data, not code. Adding a size, a topping, a topping category,
// or a promo code means adding an entry to the tables below; no pricing
// logic changes. Prices are derived from the tables at calculation time.
// ---------------------------------------------------------------------------

// Size is a pizza size and its base price.
type Size struct {
	Name      string
	BasePrice Cents
}

var sizeList = []Size{
	{Name: "Small", BasePrice: 500},  // $5
	{Name: "Medium", BasePrice: 700}, // $7
	{Name: "Large", BasePrice: 1200}, // $12
}

// Sizes returns the menu's sizes in display order.
func Sizes() []Size {
	out := make([]Size, len(sizeList))
	copy(out, sizeList)
	return out
}

// LookupSize finds a size by name, case-insensitively.
func LookupSize(name string) (Size, bool) {
	for _, s := range sizeList {
		if strings.EqualFold(s.Name, strings.TrimSpace(name)) {
			return s, true
		}
	}
	return Size{}, false
}

// Category is a topping category. A topping's price is a percentage of the
// base price of the pizza it sits on, so the category carries the rate and
// individual toppings carry no price of their own.
type Category struct {
	Name          string
	PercentOfBase int64
}

var categoryList = []Category{
	{Name: "basic", PercentOfBase: 8},
	{Name: "premium", PercentOfBase: 11},
}

// Categories returns the menu's topping categories.
func Categories() []Category {
	out := make([]Category, len(categoryList))
	copy(out, categoryList)
	return out
}

func lookupCategory(name string) (Category, bool) {
	for _, c := range categoryList {
		if c.Name == name {
			return c, true
		}
	}
	return Category{}, false
}

// Topping is a menu topping. Its price is derived from its category and the
// size of the pizza (see ToppingPrice), never stored per topping.
type Topping struct {
	Name     string
	Category string
}

var toppingList = []Topping{
	{Name: "Pepperoni", Category: "basic"},
	{Name: "Sausage", Category: "basic"},
	{Name: "Extra Cheese", Category: "basic"},
	{Name: "Onions", Category: "basic"},
	{Name: "Black Olives", Category: "basic"},
	{Name: "Green Peppers", Category: "basic"},
	{Name: "Bacon", Category: "premium"},
	{Name: "Chicken", Category: "premium"},
	{Name: "Garlic", Category: "premium"},
	{Name: "Artichoke Hearts", Category: "premium"},
	{Name: "Sun-Dried Tomatoes", Category: "premium"},
	{Name: "Pineapple", Category: "premium"},
}

// Toppings returns the menu's toppings in display order.
func Toppings() []Topping {
	out := make([]Topping, len(toppingList))
	copy(out, toppingList)
	return out
}

// normalizeName makes user input comparable to menu names: case-insensitive,
// underscores treated as spaces (so "extra_cheese" works in the CLI), and
// runs of whitespace collapsed.
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", " ")
	return strings.Join(strings.Fields(s), " ")
}

// LookupTopping finds a topping by name, tolerant of case and underscores.
func LookupTopping(name string) (Topping, bool) {
	want := normalizeName(name)
	for _, t := range toppingList {
		if normalizeName(t.Name) == want {
			return t, true
		}
	}
	return Topping{}, false
}

// ToppingPrice returns the price of one topping on a pizza of the given
// size: the category's percentage of that pizza's base price. For this menu
// the percentages land on exact cents for every size (8% and 11% of
// $5/$7/$12), so no rounding occurs here; if a future size made that
// untrue, percentOf's round-half-up rule would apply.
func ToppingPrice(size Size, t Topping) Cents {
	cat, ok := lookupCategory(t.Category)
	if !ok {
		return 0
	}
	return percentOf(size.BasePrice, cat.PercentOfBase, 100)
}

// ---------------------------------------------------------------------------
// Promo codes
//
// A promo is a data record: what it does (percent off or fixed amount off
// the pre-tax subtotal), its qualifying condition (an optional minimum
// pre-tax subtotal, strictly greater), and the codes it conflicts with.
// The pricing engine interprets these records generically, so a new promo —
// including one with its own conditions and conflicts — is a new record,
// not a new branch in the pricing logic.
// ---------------------------------------------------------------------------

// PromoKind selects how a promo's discount is computed.
type PromoKind int

const (
	PromoPercentOff PromoKind = iota // Percent% off the pre-tax subtotal
	PromoFixedOff                    // Amount off the pre-tax subtotal
)

// Promo is one promo code's full definition.
type Promo struct {
	Code    string
	Kind    PromoKind
	Percent int64 // for PromoPercentOff
	Amount  Cents // for PromoFixedOff
	HasMin  bool  // whether MinPreTaxExclusive applies
	// MinPreTaxExclusive: the promo qualifies only when the pre-tax subtotal
	// is strictly greater than this amount ("over $20" means $20.00 itself
	// does not qualify).
	MinPreTaxExclusive Cents
	Conflicts          []string // codes this promo cannot be combined with
	Description        string
}

var promoList = []Promo{
	{
		Code:        "ILOVEPIZZA",
		Kind:        PromoPercentOff,
		Percent:     10,
		Conflicts:   []string{"SAVE5"},
		Description: "10% off the pre-tax total",
	},
	{
		Code:               "SAVE5",
		Kind:               PromoFixedOff,
		Amount:             500,
		HasMin:             true,
		MinPreTaxExclusive: 2000,
		Conflicts:          []string{"ILOVEPIZZA"},
		Description:        "$5 off when the pre-tax total is over $20",
	},
}

// Promos returns all defined promo codes.
func Promos() []Promo {
	out := make([]Promo, len(promoList))
	copy(out, promoList)
	return out
}

// LookupPromo finds a promo by code, case-insensitively.
func LookupPromo(code string) (Promo, bool) {
	want := strings.ToUpper(strings.TrimSpace(code))
	for _, p := range promoList {
		if p.Code == want {
			return p, true
		}
	}
	return Promo{}, false
}

// Discount returns the discount this promo gives on a pre-tax subtotal, and
// whether the promo qualifies at that subtotal at all. A fixed discount is
// floored so the discounted subtotal never goes below $0.
func (p Promo) Discount(preTax Cents) (discount Cents, qualifies bool) {
	if p.HasMin && preTax <= p.MinPreTaxExclusive {
		return 0, false
	}
	switch p.Kind {
	case PromoPercentOff:
		return percentOf(preTax, p.Percent, 100), true
	case PromoFixedOff:
		d := p.Amount
		if d > preTax {
			d = preTax
		}
		return d, true
	}
	return 0, true
}

// ConflictsWith reports whether this promo's conflict list names the given
// (canonical) code.
func (p Promo) ConflictsWith(code string) bool {
	for _, c := range p.Conflicts {
		if c == code {
			return true
		}
	}
	return false
}

// ApplyPromo validates setting code as an order's promo when current (the
// canonical code already on the order, or "" for none) may already be set,
// and returns the canonical code to store. Mutual exclusion is enforced
// here, loudly: combining conflicting promos is an error with a clear
// message, never silent replacement or stacking. The conflict information
// comes from the promo records themselves, not from hardcoded code pairs.
func ApplyPromo(current, code string) (string, error) {
	p, ok := LookupPromo(code)
	if !ok {
		return "", fmt.Errorf("unknown promo code %q", strings.TrimSpace(code))
	}
	if current == "" || current == p.Code {
		return p.Code, nil
	}
	cur, _ := LookupPromo(current)
	if p.ConflictsWith(cur.Code) || cur.ConflictsWith(p.Code) {
		return "", fmt.Errorf("promo %s cannot be combined with %s; clear the current promo first", p.Code, cur.Code)
	}
	return "", fmt.Errorf("order already has promo %s; clear it before applying %s", cur.Code, p.Code)
}

// ---------------------------------------------------------------------------
// Orders and totals
// ---------------------------------------------------------------------------

// LineItem is one line of an order: pizzas of one size, with one set of
// toppings, in a quantity. Topping names are menu names ("Bacon"); each
// topping applies to every pizza in the line, so its price is multiplied
// by Quantity like the base price.
type LineItem struct {
	Size     string   `json:"size"`
	Quantity int      `json:"quantity"`
	Toppings []string `json:"toppings"`
}

// LineTotal validates one line item and returns its total price:
// quantity × (base price + price of each topping on that size).
func LineTotal(item LineItem) (Cents, error) {
	size, ok := LookupSize(item.Size)
	if !ok {
		return 0, fmt.Errorf("unknown size %q (choose one of: %s)", item.Size, sizeNames())
	}
	if item.Quantity < 1 {
		return 0, fmt.Errorf("quantity for a %s line must be at least 1, got %d", size.Name, item.Quantity)
	}
	perPizza := size.BasePrice
	seen := map[string]bool{}
	for _, name := range item.Toppings {
		t, ok := LookupTopping(name)
		if !ok {
			return 0, fmt.Errorf("unknown topping %q (see the menu for available toppings)", name)
		}
		if seen[t.Name] {
			return 0, fmt.Errorf("topping %s is listed twice on the same pizza", t.Name)
		}
		seen[t.Name] = true
		perPizza += ToppingPrice(size, t)
	}
	return perPizza * Cents(item.Quantity), nil
}

func sizeNames() string {
	names := make([]string, len(sizeList))
	for i, s := range sizeList {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

// TipMode selects how a tip is specified.
type TipMode string

const (
	TipNone    TipMode = ""        // no tip
	TipAmount  TipMode = "amount"  // a fixed dollar amount
	TipPercent TipMode = "percent" // a percentage of the total bill
)

// Tip is the customer's tip, in exactly one of the two modes.
type Tip struct {
	Mode    TipMode `json:"mode"`
	Amount  Cents   `json:"amount_cents,omitempty"`
	Percent int64   `json:"percent,omitempty"`
}

// amount computes the tip in cents against the total bill (the
// post-discount, post-tax total — see Price for the order of operations).
func (t Tip) amount(bill Cents) (Cents, error) {
	switch t.Mode {
	case TipNone:
		return 0, nil
	case TipAmount:
		if t.Amount < 0 {
			return 0, fmt.Errorf("tip amount cannot be negative")
		}
		return t.Amount, nil
	case TipPercent:
		if t.Percent < 0 || t.Percent > 100 {
			return 0, fmt.Errorf("tip percentage must be between 0 and 100, got %d", t.Percent)
		}
		return percentOf(bill, t.Percent, 100), nil
	}
	return 0, fmt.Errorf("unknown tip mode %q", t.Mode)
}

// Totals is the full price breakdown of an order, all in cents. Taxable is
// the post-discount subtotal that tax is computed on.
type Totals struct {
	Subtotal Cents `json:"subtotal_cents"`
	Discount Cents `json:"discount_cents"`
	Taxable  Cents `json:"taxable_cents"`
	Tax      Cents `json:"tax_cents"`
	Tip      Cents `json:"tip_cents"`
	Total    Cents `json:"total_cents"`
}

// Tax is 6.5% of the (post-discount) total order.
const (
	taxNum = 65
	taxDen = 1000
)

// ErrPromoNotQualified is returned (wrapped) when a promo code is real but
// its qualifying condition is not met by the order's pre-tax subtotal. A
// promo that does not qualify is rejected outright rather than silently
// ignored, so the customer is never surprised by a missing discount.
var ErrPromoNotQualified = errors.New("promo does not qualify for this order")

// Price computes the totals for an order. It is the one and only place
// totals are calculated.
//
// Order of operations (chosen where the spec left it open, and documented
// in the README's Assumptions):
//
//  1. Subtotal: sum of all line totals.
//  2. Promo discount, computed on the pre-tax subtotal.
//  3. Tax: 6.5% of the post-discount subtotal.
//  4. Tip: a percentage tip is a percentage of the total bill, read as the
//     post-discount, post-tax total (what the customer is actually billed
//     before tip). A dollar tip is taken as given.
//  5. Total = post-discount subtotal + tax + tip.
func Price(items []LineItem, tip Tip, promoCode string) (Totals, error) {
	if len(items) == 0 {
		return Totals{}, fmt.Errorf("order must contain at least one line item")
	}
	var subtotal Cents
	for _, item := range items {
		lt, err := LineTotal(item)
		if err != nil {
			return Totals{}, err
		}
		subtotal += lt
	}

	var discount Cents
	if strings.TrimSpace(promoCode) != "" {
		p, ok := LookupPromo(promoCode)
		if !ok {
			return Totals{}, fmt.Errorf("unknown promo code %q", strings.TrimSpace(promoCode))
		}
		d, qualifies := p.Discount(subtotal)
		if !qualifies {
			return Totals{}, fmt.Errorf("%w: %s requires a pre-tax total over %s (this order is %s)",
				ErrPromoNotQualified, p.Code, p.MinPreTaxExclusive, subtotal)
		}
		discount = d
	}

	taxable := subtotal - discount
	tax := percentOf(taxable, taxNum, taxDen)
	tipAmt, err := tip.amount(taxable + tax)
	if err != nil {
		return Totals{}, err
	}
	return Totals{
		Subtotal: subtotal,
		Discount: discount,
		Taxable:  taxable,
		Tax:      tax,
		Tip:      tipAmt,
		Total:    taxable + tax + tipAmt,
	}, nil
}

// Describe produces the written description of an order: one line per line
// item with quantity, size, and its toppings ("plain" when there are none).
func Describe(items []LineItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("order must contain at least one line item")
	}
	var b strings.Builder
	for i, item := range items {
		size, ok := LookupSize(item.Size)
		if !ok {
			return "", fmt.Errorf("unknown size %q (choose one of: %s)", item.Size, sizeNames())
		}
		names := make([]string, 0, len(item.Toppings))
		for _, name := range item.Toppings {
			t, ok := LookupTopping(name)
			if !ok {
				return "", fmt.Errorf("unknown topping %q (see the menu for available toppings)", name)
			}
			names = append(names, t.Name)
		}
		toppingText := "plain"
		if len(names) > 0 {
			toppingText = strings.Join(names, ", ")
		}
		fmt.Fprintf(&b, "%d. %d × %s pizza (%s)\n", i+1, item.Quantity, size.Name, toppingText)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
