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

// ErrIdempotencyConflict is returned (wrapped, with the original order ID)
// when an idempotency key is reused with contents that differ from the
// request that key originally created. The key is definitely already used,
// so the error always names the original order: a client that hits this
// must be able to find the order it actually has, not be stranded.
var ErrIdempotencyConflict = errors.New("idempotency key already used for a different order")

// Store holds all orders. The zero value is not usable; call New.
type Store struct {
	mu     sync.Mutex
	orders map[string]*order.Order
	byIdem map[string]idemRecord // idempotency key -> original creation record
	seq    int
}

// idemRecord remembers what an idempotency key was first used to create.
// The creation input is stored separately from the order itself because the
// order is mutable: a later PATCH changes the order but must not change what
// the key originally meant. Replay comparison is therefore always against
// the immutable original request, not the order's current contents.
type idemRecord struct {
	orderID string
	input   order.Input
}

// New returns an empty store.
func New() *Store {
	return &Store{
		orders: map[string]*order.Order{},
		byIdem: map[string]idemRecord{},
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
// If idemKey is non-empty and was seen before, Create returns the order
// that key created, with created == false, and stores nothing new: a
// customer retrying a submission after a timeout gets their original order
// ID back, never a duplicate. An empty key opts out of idempotency (every
// call creates a new order), which the API documents.
//
// Replay is compared against the *original creation request* recorded for
// that key, never against the order's current contents, so an intervening
// PATCH cannot change what a replay of the original request means. A replay
// returns the order's current state under its original ID; it does not
// resurrect the old contents.
func (s *Store) Create(in order.Input, idemKey string) (o *order.Order, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idemKey != "" {
		if rec, ok := s.byIdem[idemKey]; ok {
			if !inputsEqual(in, rec.input) {
				return nil, false, fmt.Errorf("%w: key %q created order %s; retry that exact request or use PATCH /orders/%s to change it",
					ErrIdempotencyConflict, idemKey, rec.orderID, rec.orderID)
			}
			existing, ok := s.orders[rec.orderID]
			if !ok {
				return nil, false, fmt.Errorf("%w: key %q refers to missing order %s", ErrNotFound, idemKey, rec.orderID)
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
		// Record a private deep copy of the creation request so later
		// mutation of the caller's slices cannot alter what this key means.
		s.byIdem[idemKey] = idemRecord{
			orderID: o.ID,
			input: order.Input{
				Items:     cloneItems(in.Items),
				Tip:       in.Tip,
				PromoCode: in.PromoCode,
			},
		}
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

// inputsEqual reports whether two creation requests are the same order.
// Promo codes are compared in canonical form so "save5" and "SAVE5" are one
// request; items are compared element-wise, treating a nil and an empty
// topping list as equivalent.
func inputsEqual(a, b order.Input) bool {
	if len(a.Items) != len(b.Items) {
		return false
	}
	for i := range a.Items {
		x, y := a.Items[i], b.Items[i]
		if x.Size != y.Size || x.Quantity != y.Quantity {
			return false
		}
		if len(x.Toppings) != len(y.Toppings) {
			return false
		}
		if len(x.Toppings) > 0 && !reflect.DeepEqual(x.Toppings, y.Toppings) {
			return false
		}
	}
	if a.Tip != b.Tip {
		return false
	}
	return canonicalPromo(a.PromoCode) == canonicalPromo(b.PromoCode)
}
