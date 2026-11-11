// Package order models an order and its lifecycle, independent of how it
// is stored or served. Menu items, tips, and totals themselves live in the
// pricing package; this package adds identity, status, and timestamps.
package order

import (
	"time"

	"pizzabuilder/internal/pricing"
)

// Status is where an order sits in its lifecycle.
//
// The four kitchen statuses come straight from the spec. Cancelled is a
// fifth, terminal status of a different kind: it is reached by a customer
// action, not by the kitchen, and only from Not Started. A cancelled order
// reports status "Cancelled" and rejects every further change.
type Status string

const (
	StatusNotStarted     Status = "Not Started"
	StatusStarted        Status = "Started"
	StatusReadyForPickup Status = "Ready for Pickup"
	StatusDelivered      Status = "Delivered"
	StatusCancelled      Status = "Cancelled"
)

// Next returns the status the restaurant advances this order to, and
// whether advancing is possible at all. Delivered and Cancelled are
// terminal: they have no next status.
func (s Status) Next() (Status, bool) {
	switch s {
	case StatusNotStarted:
		return StatusStarted, true
	case StatusStarted:
		return StatusReadyForPickup, true
	case StatusReadyForPickup:
		return StatusDelivered, true
	}
	return "", false
}

// Input is everything a customer supplies about an order: its line items,
// tip, and promo code. It deliberately has no totals field — totals are
// always computed by the pricing package, never supplied by a client.
type Input struct {
	Items     []pricing.LineItem `json:"items"`
	Tip       pricing.Tip        `json:"tip"`
	PromoCode string             `json:"promo_code"`
}

// Order is a submitted order: the customer's input, the totals the service
// computed for it, and its lifecycle state.
type Order struct {
	ID        string             `json:"id"`
	Items     []pricing.LineItem `json:"items"`
	Tip       pricing.Tip        `json:"tip"`
	PromoCode string             `json:"promo_code,omitempty"`
	Totals    pricing.Totals     `json:"totals"`
	Status    Status             `json:"status"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// Clone returns a deep copy, so callers can never mutate an order that the
// store is holding (or another caller's copy of it) through a shared slice.
func (o *Order) Clone() *Order {
	if o == nil {
		return nil
	}
	c := *o
	c.Items = cloneItems(o.Items)
	return &c
}

func cloneItems(items []pricing.LineItem) []pricing.LineItem {
	out := make([]pricing.LineItem, len(items))
	for i, item := range items {
		out[i] = item
		if item.Toppings != nil {
			out[i].Toppings = append([]string(nil), item.Toppings...)
		}
	}
	return out
}
