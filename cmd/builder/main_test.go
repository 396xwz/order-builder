package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"pizzabuilder/internal/order"
	"pizzabuilder/internal/pricing"
)

func TestParseMoney(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  pricing.Cents
	}{
		{"0", 0},
		{"3", 300},
		{"3.5", 350},
		{"3.50", 350},
		{"$0.01", 1},
		{"  $12.05  ", 1205},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseMoney(tt.input)
			if err != nil || got != tt.want {
				t.Errorf("parseMoney(%q) = (%d, %v), want (%d, nil)", tt.input, got, err, tt.want)
			}
		})
	}
	for _, input := range []string{"", " ", "$", "-1", "$-0.01", "abc", "1.234", "1.", ".50", "1.2.3", "3.x"} {
		t.Run("invalid/"+input, func(t *testing.T) {
			if got, err := parseMoney(input); err == nil {
				t.Errorf("parseMoney(%q) = %d without an error", input, got)
			}
		})
	}
}

func TestParseToppings(t *testing.T) {
	for _, tt := range []struct {
		name  string
		input string
		want  []string
	}{
		{"canonical names", "Bacon,Pineapple", []string{"Bacon", "Pineapple"}},
		{"case spaces and underscores", " EXTRA_cheese ,black_olives ", []string{"Extra Cheese", "Black Olives"}},
		{"empty entries", ",bacon,, ", []string{"Bacon"}},
		{"empty list", "", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseToppings(tt.input)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseToppings(%q) = (%v, %v), want (%v, nil)", tt.input, got, err, tt.want)
			}
		})
	}
	if _, err := parseToppings("bacon,not_a_topping"); err == nil {
		t.Error("unknown topping was accepted")
	}
}

func TestDescribe(t *testing.T) {
	var out bytes.Buffer
	s := &session{
		out: &out,
		items: []pricing.LineItem{
			{Size: "Large", Quantity: 2, Toppings: []string{"Bacon", "Pineapple"}},
			{Size: "Small", Quantity: 1},
		},
	}
	s.describe()
	want := "1. 2 \u00d7 Large pizza (Bacon, Pineapple)\n" +
		"2. 1 \u00d7 Small pizza (plain)\n" +
		"  Subtotal: $34.28\n" +
		"  Tax (6.5%): $2.23\n" +
		"  Total: $36.51\n"
	if got := out.String(); got != want {
		t.Errorf("describe output:\n%s\nwant:\n%s", got, want)
	}
}

func TestDescribeEmptyOrder(t *testing.T) {
	var out bytes.Buffer
	s := &session{out: &out}
	s.describe()
	if got, want := out.String(), "The order is empty.\n"; got != want {
		t.Errorf("describe output = %q, want %q", got, want)
	}
}

func TestShowTotals(t *testing.T) {
	for _, tt := range []struct {
		name  string
		items []pricing.LineItem
		tip   pricing.Tip
		promo string
		want  string
	}{
		{
			name: "empty order",
			want: "Total: $0.00 (order is empty)\n",
		},
		{
			name:  "discount tax and tip formatted as dollars",
			items: []pricing.LineItem{{Size: "Small", Quantity: 2}},
			tip:   pricing.Tip{Mode: pricing.TipAmount, Amount: 300},
			promo: "ILOVEPIZZA",
			want: "  Subtotal: $10.00\n" +
				"  Discount (ILOVEPIZZA): -$1.00\n" +
				"  Tax (6.5%): $0.59\n" +
				"  Tip: $3.00\n" +
				"  Total: $12.59\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			s := &session{out: &out, items: tt.items, tip: tt.tip, promo: tt.promo}
			s.showTotals()
			if got := out.String(); got != tt.want {
				t.Errorf("showTotals output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDispatchLocalCommands(t *testing.T) {
	var out bytes.Buffer
	s := &session{out: &out}
	for _, tt := range []struct {
		command string
		items   []pricing.LineItem
		tip     pricing.Tip
		message string
	}{
		{
			command: "add   large  2 bacon,extra_cheese",
			items:   []pricing.LineItem{{Size: "Large", Quantity: 2, Toppings: []string{"Bacon", "Extra Cheese"}}},
			message: "Added line 1.",
		},
		{
			command: "setqty 1 3",
			items:   []pricing.LineItem{{Size: "Large", Quantity: 3, Toppings: []string{"Bacon", "Extra Cheese"}}},
			message: "Total:",
		},
		{
			command: "setqty 1 0",
			items:   []pricing.LineItem{{Size: "Large", Quantity: 3, Toppings: []string{"Bacon", "Extra Cheese"}}},
			message: "Quantity must be a whole number",
		},
		{
			command: "settoppings 1 none",
			items:   []pricing.LineItem{{Size: "Large", Quantity: 3}},
			message: "Total:",
		},
		{
			command: "tip amount $3.50",
			items:   []pricing.LineItem{{Size: "Large", Quantity: 3}},
			tip:     pricing.Tip{Mode: pricing.TipAmount, Amount: 350},
			message: "Tip: $3.50",
		},
		{
			command: "tip percent 101",
			items:   []pricing.LineItem{{Size: "Large", Quantity: 3}},
			tip:     pricing.Tip{Mode: pricing.TipAmount, Amount: 350},
			message: "Tip percentage must be a whole number between 0 and 100",
		},
		{
			command: "tip percent 20",
			items:   []pricing.LineItem{{Size: "Large", Quantity: 3}},
			tip:     pricing.Tip{Mode: pricing.TipPercent, Percent: 20},
			message: "Tip: $7.67",
		},
		{
			command: "tip clear",
			items:   []pricing.LineItem{{Size: "Large", Quantity: 3}},
			message: "Total:",
		},
		{
			command: "remove 1",
			items:   []pricing.LineItem{},
			message: "Removed line 1.",
		},
	} {
		t.Run(tt.command, func(t *testing.T) {
			out.Reset()
			if s.dispatch(tt.command) {
				t.Fatal("local editing command requested exit")
			}
			if !reflect.DeepEqual(s.items, tt.items) || s.tip != tt.tip {
				t.Fatalf("state = (%+v, %+v), want (%+v, %+v)", s.items, s.tip, tt.items, tt.tip)
			}
			if !strings.Contains(out.String(), tt.message) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tt.message)
			}
		})
	}
}

func TestDispatchControlCommands(t *testing.T) {
	for _, tt := range []struct {
		command string
		exit    bool
		message string
	}{
		{"", false, ""},
		{"quit", true, ""},
		{"exit", true, ""},
		{"not-a-command", false, "Unknown command"},
	} {
		t.Run(tt.command, func(t *testing.T) {
			var out bytes.Buffer
			s := &session{out: &out}
			if got := s.dispatch(tt.command); got != tt.exit {
				t.Errorf("dispatch returned %v, want %v", got, tt.exit)
			}
			if got := out.String(); (tt.message == "" && got != "") || !strings.Contains(got, tt.message) {
				t.Errorf("output = %q, want %q", got, tt.message)
			}
		})
	}
}
func TestHasPending(t *testing.T) {
	tests := []struct {
		name       string
		idemKey    string
		orderID    string
		pendingIn  *order.Input
		wantPending bool
	}{
		{"no submission", "", "", nil, false},
		{"successful submit - has orderID", "key-1", "ord-1", nil, false},
		{"pending after timeout", "key-2", "", &order.Input{}, true},
		{"no key", "", "", &order.Input{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &session{
				idemKey:      tt.idemKey,
				orderID:      tt.orderID,
				pendingInput: tt.pendingIn,
			}
			if got := s.hasPending(); got != tt.wantPending {
				t.Errorf("hasPending() = %v, want %v", got, tt.wantPending)
			}
		})
	}
}

// TestEditsBlockedWhilePending verifies that editing commands are rejected
// when a submission is unresolved.
func TestEditsBlockedWhilePending(t *testing.T) {
	var out bytes.Buffer
	s := &session{
		out:          &out,
		idemKey:      "key-pending",
		pendingInput: &order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1}}},
	}
	blocked := []string{
		"add small 1",
		"remove 1",
		"setqty 1 2",
		"settoppings 1 bacon",
		"tip amount 3.00",
		"promo SAVE5",
	}
	for _, cmd := range blocked {
		t.Run(cmd, func(t *testing.T) {
			out.Reset()
			if s.dispatch(cmd) {
				t.Fatal("command requested exit")
			}
			if !strings.Contains(out.String(), "previous submission") {
				t.Errorf("expected 'previous submission' message, got %q", out.String())
			}
		})
	}
}
