package pricing

import (
	"errors"
	"math"
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

func TestCentsStringMinInt64(t *testing.T) {
	// String formatting must not overflow on math.MinInt64.
	c := Cents(math.MinInt64)
	s := c.String()
	if s == "" {
		t.Error("Cents(MinInt64).String() returned empty string")
	}
	// It should start with "-$".
	if s[:2] != "-$" {
		t.Errorf("Cents(MinInt64).String() = %q, want starting with \"-$\"", s)
	}
}

func TestSafeAdd(t *testing.T) {
	big := Cents(math.MaxInt64 - 10)
	if _, err := safeAdd(big, 20); err == nil {
		t.Error("safeAdd(MaxInt64-10, 20): want overflow error")
	}
	got, err := safeAdd(100, 200)
	if err != nil || got != 300 {
		t.Errorf("safeAdd(100, 200) = (%d, %v), want (300, nil)", got, err)
	}
}

func TestSafeMul(t *testing.T) {
	big := Cents(math.MaxInt64)
	if _, err := safeMul(big, 2); err == nil {
		t.Error("safeMul(MaxInt64, 2): want overflow error")
	}
	if _, err := safeMul(1, -1); err == nil {
		t.Error("safeMul(1, -1): want error for negative multiplier")
	}
	got, err := safeMul(500, 3)
	if err != nil || got != 1500 {
		t.Errorf("safeMul(500, 3) = (%d, %v), want (1500, nil)", got, err)
	}
}

func TestLineTotalOverflow(t *testing.T) {
	// A Large pizza (1200¢) with maximum quantity should succeed.
	_, err := LineTotal(LineItem{Size: "Large", Quantity: MaxQuantity})
	if err != nil {
		t.Errorf("LineTotal with MaxQuantity: unexpected error: %v", err)
	}
	// A Large pizza with quantity just above MaxQuantity should fail.
	_, err = LineTotal(LineItem{Size: "Large", Quantity: MaxQuantity + 1})
	if err == nil {
		t.Error("LineTotal with MaxQuantity+1: want error, got nil")
	}
}

func TestPriceOverflow(t *testing.T) {
	// A single item with an extremely large quantity that would overflow
	// arithmetic should be rejected.
	big := []LineItem{{Size: "Small", Quantity: 1_000_000_000}}
	_, err := Price(big, Tip{}, "")
	if err == nil {
		t.Error("Price with overflow quantity: want error, got nil")
	}
}

// The declared contracts of the safe* helpers, including the cases the
// earlier implementations got wrong.
func TestSafeHelperContracts(t *testing.T) {
	t.Run("safeMul rejects a negative multiplier even when a is zero", func(t *testing.T) {
		// A short-circuit on a == 0 would hide this, making input
		// validation depend on unrelated data.
		if got, err := safeMul(0, -1); err == nil {
			t.Errorf("safeMul(0, -1) = (%d, nil), want an error", got)
		}
	})

	t.Run("safeMul bounds a negative a against the negative limit", func(t *testing.T) {
		// Comparing against MaxInt64/a with a negative a yields a negative
		// quotient, which would accept every b and wrap.
		if got, err := safeMul(Cents(math.MinInt64/2), 4); err == nil {
			t.Errorf("safeMul(MinInt64/2, 4) = (%d, nil), want an overflow error", got)
		}
		// A legal negative multiplication still works.
		if got, err := safeMul(-100, 3); err != nil || got != -300 {
			t.Errorf("safeMul(-100, 3) = (%d, %v), want (-300, nil)", got, err)
		}

		// The negative range extends one further than the positive one, so
		// these products are exactly representable and must be accepted.
		// Bounding by |a| would reject them (|MinInt64| is not an int64).
		if got, err := safeMul(Cents(math.MinInt64), 1); err != nil || got != Cents(math.MinInt64) {
			t.Errorf("safeMul(MinInt64, 1) = (%d, %v), want (%d, nil)", got, err, Cents(math.MinInt64))
		}
		if got, err := safeMul(Cents(math.MinInt64), 0); err != nil || got != 0 {
			t.Errorf("safeMul(MinInt64, 0) = (%d, %v), want (0, nil)", got, err)
		}
		if got, err := safeMul(Cents(math.MinInt64/2), 2); err != nil || got != Cents(math.MinInt64) {
			t.Errorf("safeMul(MinInt64/2, 2) = (%d, %v), want (%d, nil)", got, err, Cents(math.MinInt64))
		}
		// One step past each of those boundaries must still be rejected.
		if got, err := safeMul(Cents(math.MinInt64), 2); err == nil {
			t.Errorf("safeMul(MinInt64, 2) = (%d, nil), want an overflow error", got)
		}
		if got, err := safeMul(Cents(math.MinInt64/2), 3); err == nil {
			t.Errorf("safeMul(MinInt64/2, 3) = (%d, nil), want an overflow error", got)
		}
		// The smallest value whose product with 2 wraps is MinInt64/2 - 1.
		if got, err := safeMul(Cents(math.MinInt64/2-1), 2); err == nil {
			t.Errorf("safeMul(MinInt64/2-1, 2) = (%d, nil), want an overflow error", got)
		}
		// a == -1 is the one case where computing the limit as
		// MinInt64/a overflows the division itself (Go defines the result
		// as MinInt64), which would reject every multiplier.
		if got, err := safeMul(-1, 2); err != nil || got != -2 {
			t.Errorf("safeMul(-1, 2) = (%d, %v), want (-2, nil)", got, err)
		}
		if got, err := safeMul(-1, math.MaxInt64); err != nil || got != Cents(-math.MaxInt64) {
			t.Errorf("safeMul(-1, MaxInt64) = (%d, %v), want (%d, nil)", got, err, Cents(-math.MaxInt64))
		}

		// The positive boundary is unchanged: MaxInt64 × 1 is exact.
		if got, err := safeMul(Cents(math.MaxInt64), 1); err != nil || got != Cents(math.MaxInt64) {
			t.Errorf("safeMul(MaxInt64, 1) = (%d, %v), want (%d, nil)", got, err, Cents(math.MaxInt64))
		}
	})

	t.Run("safePercentOf validates its rate arguments", func(t *testing.T) {
		if _, err := safePercentOf(100, 10, 0); err == nil {
			t.Error("safePercentOf with den == 0: want an error, got nil")
		}
		if _, err := safePercentOf(100, 10, -100); err == nil {
			t.Error("safePercentOf with negative den: want an error, got nil")
		}
		if _, err := safePercentOf(100, -10, 100); err == nil {
			t.Error("safePercentOf with a negative rate: want an error, got nil")
		}
	})

	t.Run("safePercentOf accounts for the rounding term", func(t *testing.T) {
		// |c|*num fits exactly, but percentOf also adds den/2 before
		// dividing; a check that ignored that term would accept this and
		// overflow inside percentOf.
		den := int64(1000)
		c := Cents((math.MaxInt64 - den/2) / 65 * 65)
		if _, err := safePercentOf(c+1000, 65, den); err == nil {
			t.Error("safePercentOf at the rounding boundary: want an overflow error, got nil")
		}
		// Well inside the bound still succeeds.
		if got, err := safePercentOf(1428, 65, 1000); err != nil || got != 93 {
			t.Errorf("safePercentOf(1428, 65, 1000) = (%d, %v), want (93, nil)", got, err)
		}
	})

	// percentOf's round-half-up rule only holds for a non-negative input:
	// the `+ den/2` term rounds a negative value half toward zero instead
	// (10% of -$10.00 would yield -99¢, not -100¢). Rather than return a
	// figure rounded by an undocumented second rule, safePercentOf refuses.
	t.Run("safePercentOf rejects a negative amount", func(t *testing.T) {
		if got, err := safePercentOf(-1000, 10, 100); err == nil {
			t.Errorf("safePercentOf(-1000, 10, 100) = (%d, nil), want an error", got)
		}
		if got, err := safePercentOf(0, 10, 100); err != nil || got != 0 {
			t.Errorf("safePercentOf(0, 10, 100) = (%d, %v), want (0, nil)", got, err)
		}
	})
}

// The documented limits are enforced at their exact boundaries.
func TestDeclaredLimitBoundaries(t *testing.T) {
	t.Run("quantity", func(t *testing.T) {
		if _, err := LineTotal(LineItem{Size: "Small", Quantity: MaxQuantity}); err != nil {
			t.Errorf("quantity at MaxQuantity: unexpected error %v", err)
		}
		if _, err := LineTotal(LineItem{Size: "Small", Quantity: MaxQuantity + 1}); err == nil {
			t.Error("quantity at MaxQuantity+1: want an error, got nil")
		}
		if _, err := LineTotal(LineItem{Size: "Small", Quantity: 0}); err == nil {
			t.Error("quantity 0: want an error, got nil")
		}
		if _, err := LineTotal(LineItem{Size: "Small", Quantity: -1}); err == nil {
			t.Error("negative quantity: want an error, got nil")
		}
	})

	t.Run("tip amount", func(t *testing.T) {
		items := []LineItem{{Size: "Small", Quantity: 1}}
		if _, err := Price(items, Tip{Mode: TipAmount, Amount: MaxCents}, ""); err == nil {
			t.Error("a tip of MaxCents pushes the total over the limit: want an error")
		}
		if _, err := Price(items, Tip{Mode: TipAmount, Amount: MaxCents + 1}, ""); err == nil {
			t.Error("tip above MaxCents: want an error, got nil")
		}
	})

	// A subtotal inside the limit can still produce a total above it once
	// tax and a dollar tip are added; the final charge must be bounded too.
	t.Run("total is bounded, not only the subtotal", func(t *testing.T) {
		// 1,000,000 Large pizzas = $12,000,000.00 — a legal subtotal.
		items := []LineItem{{Size: "Large", Quantity: MaxQuantity}}
		totals, err := Price(items, Tip{}, "")
		if err != nil {
			t.Fatalf("legal large order rejected: %v", err)
		}
		if totals.Subtotal > MaxCents || totals.Total > MaxCents {
			t.Errorf("totals exceed MaxCents: %+v", totals)
		}
		// Now add a tip that pushes the *total* past the limit while the
		// subtotal stays legal.
		if _, err := Price(items, Tip{Mode: TipAmount, Amount: MaxCents - 1}, ""); err == nil {
			t.Error("a total above MaxCents was accepted; want an error")
		}
	})

	t.Run("every reported figure stays within the limits", func(t *testing.T) {
		items := []LineItem{{Size: "Large", Quantity: 3, Toppings: []string{"Bacon", "Pepperoni"}}}
		totals, err := Price(items, Tip{Mode: TipPercent, Percent: 100}, "")
		if err != nil {
			t.Fatalf("Price: %v", err)
		}
		for name, v := range map[string]Cents{
			"subtotal": totals.Subtotal,
			"discount": totals.Discount,
			"taxable":  totals.Taxable,
			"tax":      totals.Tax,
			"tip":      totals.Tip,
			"total":    totals.Total,
		} {
			if v < 0 || v > MaxCents {
				t.Errorf("%s = %s, outside [0, %s]", name, v, MaxCents)
			}
		}
	})
}

func TestPercentOfOverflow(t *testing.T) {
	// safePercentOf should reject overflow cases.
	// With 10% of MaxInt64, the multiplication overflows.
	big := Cents(math.MaxInt64/10 + 1)
	_, err := safePercentOf(big, 10, 100)
	if err == nil {
		t.Error("safePercentOf(just-over-MaxInt64/10, 10, 100): want overflow error")
	}
	// Normal case should work.
	got, err := safePercentOf(1000, 10, 100)
	if err != nil || got != 100 {
		t.Errorf("safePercentOf(1000, 10, 100) = (%d, %v), want (100, nil)", got, err)
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
