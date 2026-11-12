// Package store is the order service's in-memory storage. It owns every
// lifecycle rule: which operations are legal in which status, idempotent
// submission, and atomicity of competing operations.
//
// Concurrency model: one mutex guards the whole store, and every operation
// holds it for its entire check-and-mutate sequence. That is what makes
// the lifecycle guarantees hold under races — when a cancel and a kitchen
// advance arrive at the same moment, one of them acquires the lock first
// and completes; the other then observes the new status and is rejected.
// Exactly one wins, always. (A production system would get the same
// guarantee from a database transaction or a conditional update; the
// invariant being demonstrated is identical.)
package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"pizzabuilder/internal/order"
	"pizzabuilder/internal/pricing"
)

// ErrNotFound is returned when no order has the given ID.
var ErrNotFound = errors.New("order not found")

// ErrStateConflict is returned (wrapped, with the order's current status)
// when an operation is not allowed in the order's current status — e.g.
// updating or cancelling an order the kitchen has already started.
var ErrStateConflict = errors.New("operation not allowed in the order's current state")

// Store holds all orders. The zero value is not usable; call New.
type Store struct {
	mu     sync.Mutex
	orders map[string]*order.Order
	byIdem map[string]string // idempotency key -> order ID
	seq    int
}

// New returns an empty store.
func New() *Store {
	return &Store{
		orders: map[string]*order.Order{},
		byIdem: map[string]string{},
	}
}

// Count returns how many orders the store holds.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.orders)
}

// Create validates and stores a new order, computing its totals with the
// pricing package (client-supplied totals are never accepted — Input has
// no totals field). The order starts life as Not Started.
//
// If idemKey is non-empty and was seen before, Create returns the original
// order with created == false and stores nothing new: a customer retrying
// a submission after a timeout gets their original order ID back, never a
// duplicate. An empty key opts out of idempotency (every call creates a
// new order), which the API documents.
func (s *Store) Create(in order.Input, idemKey string) (o *order.Order, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idemKey != "" {
		if id, ok := s.byIdem[idemKey]; ok {
			existing := s.orders[id]
			if !inputsEqual(in, existing) {
				return nil, false, fmt.Errorf("idempotency key %q already used for order %q with different contents; use PATCH /orders/%s to update", idemKey, id, id)
			}
			return existing.Clone(), false, nil
		}
	}
	totals, err := pricing.Price(in.Items, in.Tip, in.PromoCode)
	if err != nil {
		return nil, false, err
	}
	s.seq++
	now := time.Now().UTC()
	o = &order.Order{
		ID:        fmt.Sprintf("ord-%04d", s.seq),
		Items:     cloneItems(in.Items),
		Tip:       in.Tip,
		PromoCode: canonicalPromo(in.PromoCode),
		Totals:    totals,
		Status:    order.StatusNotStarted,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.orders[o.ID] = o
	if idemKey != "" {
		s.byIdem[idemKey] = o.ID
	}
	return o.Clone(), true, nil
}

// Get returns a copy of the order with the given ID.
func (s *Store) Get(id string) (*order.Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return nil, false
	}
	return o.Clone(), true
}

// Update replaces an order's items, tip, and promo wholesale (there are no
// partial updates) and recomputes its totals. It is legal only while the
// order is Not Started; after that it fails with ErrStateConflict.
func (s *Store) Update(id string, in order.Input) (*order.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, ok := s.orders[id]
	if !ok {
		return nil, ErrNotFound
	}
	if o.Status != order.StatusNotStarted {
		return nil, fmt.Errorf("%w: order %s is %q and can only be changed while %q",
			ErrStateConflict, id, o.Status, order.StatusNotStarted)
	}
	totals, err := pricing.Price(in.Items, in.Tip, in.PromoCode)
	if err != nil {
		return nil, err
	}
	o.Items = cloneItems(in.Items)
	o.Tip = in.Tip
	o.PromoCode = canonicalPromo(in.PromoCode)
	o.Totals = totals
	o.UpdatedAt = time.Now().UTC()
	return o.Clone(), nil
}

// Cancel cancels an order. It is legal only while the order is Not
// Started; after that it fails with ErrStateConflict.
func (s *Store) Cancel(id string) (*order.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, ok := s.orders[id]
	if !ok {
		return nil, ErrNotFound
	}
	if o.Status != order.StatusNotStarted {
		return nil, fmt.Errorf("%w: order %s is %q and can only be cancelled while %q",
			ErrStateConflict, id, o.Status, order.StatusNotStarted)
	}
	o.Status = order.StatusCancelled
	o.UpdatedAt = time.Now().UTC()
	return o.Clone(), nil
}

// Advance moves an order one step along the kitchen lifecycle
// (Not Started -> Started -> Ready for Pickup -> Delivered). Advancing a
// Delivered or Cancelled order fails with ErrStateConflict.
func (s *Store) Advance(id string) (*order.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, ok := s.orders[id]
	if !ok {
		return nil, ErrNotFound
	}
	next, ok := o.Status.Next()
	if !ok {
		return nil, fmt.Errorf("%w: order %s is %q and cannot be advanced", ErrStateConflict, id, o.Status)
	}
	o.Status = next
	o.UpdatedAt = time.Now().UTC()
	return o.Clone(), nil
}

// AdvanceAll advances every order that can be advanced by one step and
// returns how many moved. It backs the order service's optional
// auto-advance simulation of the kitchen.
func (s *Store) AdvanceAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	moved := 0
	for _, o := range s.orders {
		if next, ok := o.Status.Next(); ok {
			o.Status = next
			o.UpdatedAt = time.Now().UTC()
			moved++
		}
	}
	return moved
}

// canonicalPromo returns the canonical (uppercase) form of a promo code
// that pricing.Price has already validated, or "" for none.
func canonicalPromo(code string) string {
	if strings.TrimSpace(code) == "" {
		return ""
	}
	if p, ok := pricing.LookupPromo(code); ok {
		return p.Code
	}
	return strings.ToUpper(strings.TrimSpace(code))
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

// inputsEqual reports whether the given input matches the order's current
// items, tip, and promo code. Used to detect same-key/different-body misuse.
func inputsEqual(in order.Input, o *order.Order) bool {
	if !reflect.DeepEqual(in.Items, o.Items) {
		return false
	}
	if in.Tip != o.Tip {
		return false
	}
	if canonicalPromo(in.PromoCode) != o.PromoCode {
		return false
	}
	return true
}
