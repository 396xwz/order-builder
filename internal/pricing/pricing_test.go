package pricing

import (
	"errors"
	"strings"
	"testing"
)

// The expected topping prices below are the spec's percentages worked out
// by hand: basic = 8% of base, premium = 11% of base.
func TestToppingPricesEverySizeAndCategory(t *testing.T) {
	want := map[string]map[string]Cents{
		"Small":  {"basic": 40, "premium": 55},  // 8%/11% of $5.00
		"Medium": {"basic": 56, "premium": 77},  // 8%/11% of $7.00
		"Large":  {"basic": 96, "premium": 132}, // 8%/11% of $12.00
	}
	for _, size := range Sizes() {
		for _, cat := range Categories() {
			// Find a topping in this category to price.
			var top Topping
			for _, tp := range Toppings() {
				if tp.Category == cat.Name {
					top = tp
					break
				}
			}
			got := ToppingPrice(size, top)
			if got != want[size.Name][cat.Name] {
				t.Errorf("%s %s topping: got %s, want %s", size.Name, cat.Name, got, want[size.Name][cat.Name])
			}
			// The spec's percentages land on exact cents for every size;
			// assert that so a future size that breaks it is caught here
			// rather than silently rounded.
			if (int64(size.BasePrice)*cat.PercentOfBase)%100 != 0 {
				t.Errorf("%s %s: percentage does not land on exact cents", size.Name, cat.Name)
			}
		}
	}
}

func TestPercentOfRoundsHalfUp(t *testing.T) {
	cases := []struct {
		amount   Cents
		num, den int64
		want     Cents
	}{
		{100, 65, 1000, 7},   // 6.5% of $1.00 = 6.5c -> 7c (half rounds up)
		{1428, 65, 1000, 93}, // 6.5% of $14.28 = 92.82c -> 93c
		{2012, 10, 100, 201}, // 10% of $20.12 = 201.2c -> 201c
		{1521, 20, 100, 304}, // 20% of $15.21 = 304.2c -> 304c
		{1000, 10, 100, 100}, // exact values stay exact
	}
	for _, c := range cases {
		if got := percentOf(c.amount, c.num, c.den); got != c.want {
			t.Errorf("percentOf(%d, %d, %d) = %d, want %d", c.amount, c.num, c.den, got, c.want)
		}
	}
}

// Full orders with every number computed by hand from the spec, independent
// of the implementation's helper functions.
func TestPriceHandComputedOrders(t *testing.T) {
	cases := []struct {
		name  string
		items []LineItem
		tip   Tip
		promo string
		want  Totals
	}{
		{
			name:  "large with basic and premium topping, 20% tip, no promo",
			items: []LineItem{{Size: "Large", Quantity: 1, Toppings: []string{"Pepperoni", "Bacon"}}},
			tip:   Tip{Mode: TipPercent, Percent: 20},
			// subtotal 1200 + 96 + 132 = 1428; tax 6.5% of 1428 = 92.82 -> 93;
			// tip 20% of bill (1428 + 93 = 1521) = 304.2 -> 304; total 1825.
			want: Totals{Subtotal: 1428, Discount: 0, Taxable: 1428, Tax: 93, Tip: 304, Total: 1825},
		},
		{
			name: "two mediums and a small, SAVE5, $3 tip",
			items: []LineItem{
				{Size: "Medium", Quantity: 2, Toppings: []string{"Extra Cheese"}},
				{Size: "Small", Quantity: 1},
			},
			tip:   Tip{Mode: TipAmount, Amount: 300},
			promo: "SAVE5",
			// subtotal (700 + 56) * 2 + 500 = 2012; SAVE5 -> -500; taxable 1512;
			// tax 6.5% of 1512 = 98.28 -> 98; total 1512 + 98 + 300 = 1910.
			want: Totals{Subtotal: 2012, Discount: 500, Taxable: 1512, Tax: 98, Tip: 300, Total: 1910},
		},
		{
			name: "same items with ILOVEPIZZA and 10% tip",
			items: []LineItem{
				{Size: "Medium", Quantity: 2, Toppings: []string{"Extra Cheese"}},
				{Size: "Small", Quantity: 1},
			},
			tip:   Tip{Mode: TipPercent, Percent: 10},
			promo: "ILOVEPIZZA",
			// subtotal 2012; discount 10% = 201.2 -> 201; taxable 1811;
			// tax 6.5% of 1811 = 117.715 -> 118; tip 10% of (1811 + 118 = 1929)
			// = 192.9 -> 193; total 1811 + 118 + 193 = 2122.
			want: Totals{Subtotal: 2012, Discount: 201, Taxable: 1811, Tax: 118, Tip: 193, Total: 2122},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Price(c.items, c.tip, c.promo)
			if err != nil {
				t.Fatalf("Price returned error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// "Over $20" is strict: a pre-tax total of exactly $20.00 does not qualify
// for SAVE5, and $20.01 does. The boundary is tested at the promo level
// (where the condition lives) because no combination of menu prices sums
// to $20.01; the $20.00 case is also tested end-to-end through Price.
func TestSave5ThresholdBoundary(t *testing.T) {
	save5, ok := LookupPromo("SAVE5")
	if !ok {
		t.Fatal("SAVE5 promo not defined")
	}
	if _, qualifies := save5.Discount(2000); qualifies {
		t.Error("SAVE5 qualified at exactly $20.00; spec says over $20")
	}
	if d, qualifies := save5.Discount(2001); !qualifies || d != 500 {
		t.Errorf("SAVE5 at $20.01: got discount %s, qualifies %v; want $5.00, true", d, qualifies)
	}

	// End to end: 4 small pizzas = exactly $20.00.
	_, err := Price([]LineItem{{Size: "Small", Quantity: 4}}, Tip{}, "SAVE5")
	if !errors.Is(err, ErrPromoNotQualified) {
		t.Errorf("Price with $20.00 order and SAVE5: got err %v, want ErrPromoNotQualified", err)
	}
}

func TestPromoMutualExclusion(t *testing.T) {
	if code, err := ApplyPromo("", "save5"); err != nil || code != "SAVE5" {
		t.Errorf("ApplyPromo(none, save5) = %q, %v; want SAVE5, nil", code, err)
	}
	if code, err := ApplyPromo("SAVE5", "SAVE5"); err != nil || code != "SAVE5" {
		t.Errorf("re-applying the same promo = %q, %v; want SAVE5, nil", code, err)
	}
	for _, pair := range [][2]string{{"ILOVEPIZZA", "SAVE5"}, {"SAVE5", "ILOVEPIZZA"}} {
		_, err := ApplyPromo(pair[0], pair[1])
		if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Errorf("ApplyPromo(%s, %s): got err %v, want a 'cannot be combined' error", pair[0], pair[1], err)
		}
	}
	if _, err := ApplyPromo("", "FREEBIE"); err == nil {
		t.Error("ApplyPromo with unknown code: got nil error")
	}
	// The conflict declarations themselves must be symmetric: if A names B,
	// B names A. Asymmetric data would make exclusion depend on order.
	for _, p := range Promos() {
		for _, other := range p.Conflicts {
			o, ok := LookupPromo(other)
			if !ok {
				t.Fatalf("promo %s conflicts with unknown code %s", p.Code, other)
			}
			if !o.ConflictsWith(p.Code) {
				t.Errorf("promo conflict not symmetric: %s lists %s but not vice versa", p.Code, other)
			}
		}
	}
}

func TestPriceRejectsInvalidInput(t *testing.T) {
	valid := []LineItem{{Size: "Small", Quantity: 1}}
	cases := []struct {
		name  string
		items []LineItem
		tip   Tip
		promo string
	}{
		{"no line items", nil, Tip{}, ""},
		{"unknown size", []LineItem{{Size: "Huge", Quantity: 1}}, Tip{}, ""},
		{"unknown topping", []LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Gold Flakes"}}}, Tip{}, ""},
		{"zero quantity", []LineItem{{Size: "Small", Quantity: 0}}, Tip{}, ""},
		{"duplicate topping", []LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Bacon", "bacon"}}}, Tip{}, ""},
		{"unknown promo", valid, Tip{}, "HALFOFF"},
		{"tip percent over 100", valid, Tip{Mode: TipPercent, Percent: 101}, ""},
		{"negative tip amount", valid, Tip{Mode: TipAmount, Amount: -100}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Price(c.items, c.tip, c.promo); err == nil {
				t.Error("Price accepted invalid input, want an error")
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	desc, err := Describe([]LineItem{
		{Size: "Large", Quantity: 2, Toppings: []string{"Bacon", "Pineapple"}},
		{Size: "Small", Quantity: 1},
	})
	if err != nil {
		t.Fatalf("Describe returned error: %v", err)
	}
	if !strings.Contains(desc, "2 × Large pizza (Bacon, Pineapple)") {
		t.Errorf("description missing first line:\n%s", desc)
	}
	if !strings.Contains(desc, "1 × Small pizza (plain)") {
		t.Errorf("description missing plain line:\n%s", desc)
	}
}
