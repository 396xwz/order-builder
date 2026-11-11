package order

import (
	"reflect"
	"testing"
	"time"

	"pizzabuilder/internal/pricing"
)

func TestStatusNext(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		next   Status
		ok     bool
	}{
		{"not started", StatusNotStarted, StatusStarted, true},
		{"started", StatusStarted, StatusReadyForPickup, true},
		{"ready for pickup", StatusReadyForPickup, StatusDelivered, true},
		{"delivered is terminal", StatusDelivered, "", false},
		{"cancelled is terminal", StatusCancelled, "", false},
		{"zero status", "", "", false},
		{"unknown status", "unknown", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if next, ok := tt.status.Next(); next != tt.next || ok != tt.ok {
				t.Errorf("Next() = (%q, %v), want (%q, %v)", next, ok, tt.next, tt.ok)
			}
		})
	}
}

func TestClone(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	original := &Order{
		ID: "ord-0001",
		Items: []pricing.LineItem{
			{Size: "Large", Quantity: 2, Toppings: []string{"Bacon", "Onions"}},
			{Size: "Small", Quantity: 1},
		},
		Tip:       pricing.Tip{Mode: pricing.TipAmount, Amount: 300},
		PromoCode: "SAVE5",
		Totals:    pricing.Totals{Subtotal: 3000, Discount: 500, Taxable: 2500, Tax: 163, Tip: 300, Total: 2963},
		Status:    StatusStarted,
		CreatedAt: now,
		UpdatedAt: now.Add(time.Minute),
	}
	cloned := original.Clone()
	if cloned == original || !reflect.DeepEqual(cloned, original) {
		t.Fatalf("Clone() = %+v, want equal contents at a different address", cloned)
	}

	cloned.ID = "changed"
	cloned.Items[0].Quantity = 9
	cloned.Items[0].Toppings[0] = "Chicken"
	cloned.Items[1].Toppings = []string{"Garlic"}
	if original.ID != "ord-0001" || original.Items[0].Quantity != 2 ||
		original.Items[0].Toppings[0] != "Bacon" || original.Items[1].Toppings != nil {
		t.Fatalf("mutating clone changed original: %+v", original)
	}
	original.Items[0].Size = "Medium"
	original.Items[0].Toppings[1] = "Pineapple"
	if cloned.Items[0].Size != "Large" || cloned.Items[0].Toppings[1] != "Onions" {
		t.Error("mutating original changed clone's items or toppings")
	}
}

func TestCloneNilAndEmpty(t *testing.T) {
	var absent *Order
	if absent.Clone() != nil {
		t.Error("nil order should clone to nil")
	}
	for _, tt := range []struct {
		name  string
		items []pricing.LineItem
	}{
		{"nil items", nil},
		{"empty items", []pricing.LineItem{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := &Order{Items: tt.items}
			cloned := original.Clone()
			if cloned == nil || cloned == original || len(cloned.Items) != 0 {
				t.Errorf("Clone() = %+v, want an independent order with no items", cloned)
			}
		})
	}
}
