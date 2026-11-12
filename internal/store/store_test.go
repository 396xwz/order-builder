package store

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"pizzabuilder/internal/order"
	"pizzabuilder/internal/pricing"
)

func smallOrder() order.Input {
	return order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1}}}
}

func mustCreate(t *testing.T, s *Store, in order.Input, key string) *order.Order {
	t.Helper()
	o, created, err := s.Create(in, key)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if !created {
		t.Fatal("Create reported created == false for a new key")
	}
	return o
}

func TestIdempotentCreate(t *testing.T) {
	s := New()
	first := mustCreate(t, s, smallOrder(), "key-123")

	second, created, err := s.Create(smallOrder(), "key-123")
	if err != nil {
		t.Fatalf("replayed Create returned error: %v", err)
	}
	if created {
		t.Error("replayed Create reported created == true")
	}
	if second.ID != first.ID {
		t.Errorf("replayed Create returned order %s, want original %s", second.ID, first.ID)
	}
	if s.Count() != 1 {
		t.Errorf("store holds %d orders after idempotent replay, want 1", s.Count())
	}
}

func TestCreateWithoutKeyAlwaysCreates(t *testing.T) {
	s := New()
	mustCreate(t, s, smallOrder(), "")
	mustCreate(t, s, smallOrder(), "")
	if s.Count() != 2 {
		t.Errorf("store holds %d orders, want 2 (no key = no idempotency)", s.Count())
	}
}

func TestIdempotentCreateDifferentBodyRejected(t *testing.T) {
	s := New()
	in1 := smallOrder()
	o, created, err := s.Create(in1, "key-123")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if !created {
		t.Fatal("expected created == true")
	}

	// Same key, different contents: should be rejected.
	in2 := order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 1}}}
	_, created, err = s.Create(in2, "key-123")
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different body with same key: got %v, want ErrIdempotencyConflict", err)
	}
	if created {
		t.Fatal("created should be false for rejected replay")
	}
	if !strings.Contains(err.Error(), o.ID) {
		t.Errorf("error should mention the original order ID %q so the client is not stranded, got %v", o.ID, err)
	}
	if s.Count() != 1 {
		t.Errorf("store holds %d orders, want 1", s.Count())
	}
}

// A replay is judged against the request the key originally created, not
// against the order's current contents. Otherwise an intervening PATCH
// would invert the comparison: the true original request would start being
// rejected, and the post-update contents would start being accepted.
func TestReplayComparedAgainstOriginalCreationInput(t *testing.T) {
	s := New()
	original := smallOrder()
	o := mustCreate(t, s, original, "key-K")

	// The customer updates the order through the permitted path.
	updated := order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 1}}}
	if _, err := s.Update(o.ID, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Replaying the *original* request must still be recognised as the
	// same submission and return the same order.
	replay, created, err := s.Create(original, "key-K")
	if err != nil {
		t.Fatalf("replay of the original request: %v", err)
	}
	if created {
		t.Error("replay reported created == true")
	}
	if replay.ID != o.ID {
		t.Errorf("replay returned %s, want original %s", replay.ID, o.ID)
	}
	// It returns the order's current state, not the stale contents.
	if len(replay.Items) != 1 || replay.Items[0].Size != "Large" {
		t.Errorf("replay should return current state (Large), got %+v", replay.Items)
	}

	// Submitting the *updated* contents under the same key is a different
	// creation request and must be refused.
	if _, _, err := s.Create(updated, "key-K"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("post-update contents under the same key: got %v, want ErrIdempotencyConflict", err)
	}
	if s.Count() != 1 {
		t.Errorf("store holds %d orders, want 1", s.Count())
	}
}

func TestReplayWorksInTerminalStates(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(t *testing.T, s *Store, id string)
	}{
		{"cancelled", func(t *testing.T, s *Store, id string) {
			if _, err := s.Cancel(id); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		}},
		{"delivered", func(t *testing.T, s *Store, id string) {
			for i := 0; i < 3; i++ {
				if _, err := s.Advance(id); err != nil {
					t.Fatalf("Advance: %v", err)
				}
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New()
			in := smallOrder()
			o := mustCreate(t, s, in, "key-T")
			tt.prepare(t, s, o.ID)

			// A late retry must still find the original order rather than
			// creating a second one behind the customer's back.
			replay, created, err := s.Create(in, "key-T")
			if err != nil {
				t.Fatalf("replay in %s state: %v", tt.name, err)
			}
			if created {
				t.Error("replay reported created == true")
			}
			if replay.ID != o.ID {
				t.Errorf("replay returned %s, want %s", replay.ID, o.ID)
			}
			if s.Count() != 1 {
				t.Errorf("store holds %d orders, want 1", s.Count())
			}
		})
	}
}

// The recorded creation request must be a private copy: mutating the
// caller's slices afterwards cannot change what the key means.
func TestIdempotencyRecordIsIsolatedFromCaller(t *testing.T) {
	s := New()
	in := order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Bacon"}}}}
	o := mustCreate(t, s, in, "key-M")

	// Mutate the caller's input after creation.
	in.Items[0].Toppings[0] = "Pineapple"

	// Replaying the true original request must still match.
	originalAgain := order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Bacon"}}}}
	replay, created, err := s.Create(originalAgain, "key-M")
	if err != nil {
		t.Fatalf("replay after caller mutation: %v", err)
	}
	if created || replay.ID != o.ID {
		t.Errorf("replay returned (%s, created=%v), want (%s, false)", replay.ID, created, o.ID)
	}
}

func TestInputsEqual(t *testing.T) {
	base := smallOrder()
	if !inputsEqual(base, smallOrder()) {
		t.Error("identical inputs should be equal")
	}
	// A nil and an empty topping list describe the same pizza.
	nilTops := order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: nil}}}
	emptyTops := order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: []string{}}}}
	if !inputsEqual(nilTops, emptyTops) {
		t.Error("nil and empty topping lists should be equal")
	}
	// Promo comparison is case-insensitive via canonical form.
	lower := order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 3}}, PromoCode: "save5"}
	upper := order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 3}}, PromoCode: "SAVE5"}
	if !inputsEqual(lower, upper) {
		t.Error("promo codes differing only in case should be equal")
	}

	for _, tt := range []struct {
		name string
		in   order.Input
	}{
		{"different size", order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 1}}}},
		{"different quantity", order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 2}}}},
		{"extra item", order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1}, {Size: "Small", Quantity: 1}}}},
		{"added topping", order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Bacon"}}}}},
		{"added promo", order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1}}, PromoCode: "ILOVEPIZZA"}},
		{"added tip", order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1}}, Tip: pricing.Tip{Mode: pricing.TipAmount, Amount: 100}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if inputsEqual(base, tt.in) {
				t.Errorf("%s should not be equal to the base input", tt.name)
			}
		})
	}
}
func TestCreateRejectsInvalidOrder(t *testing.T) {
	s := New()
	_, _, err := s.Create(order.Input{Items: []pricing.LineItem{{Size: "Huge", Quantity: 1}}}, "k")
	if err == nil {
		t.Error("Create accepted an unknown size")
	}
	if s.Count() != 0 {
		t.Error("invalid order was stored")
	}
}

func TestUpdateOnlyWhileNotStarted(t *testing.T) {
	s := New()
	o := mustCreate(t, s, smallOrder(), "")

	// Legal update: change the order, totals are recomputed by the store.
	bigger := order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 1}}}
	updated, err := s.Update(o.ID, bigger)
	if err != nil {
		t.Fatalf("Update while Not Started returned error: %v", err)
	}
	if updated.Totals.Total != 1200+78 { // $12.00 + 6.5% tax (78c)
		t.Errorf("updated total = %s, want $12.78", updated.Totals.Total)
	}

	// Once the kitchen starts it, updates are rejected.
	if _, err := s.Advance(o.ID); err != nil {
		t.Fatalf("Advance returned error: %v", err)
	}
	if _, err := s.Update(o.ID, bigger); !errors.Is(err, ErrStateConflict) {
		t.Errorf("Update after Started: got err %v, want ErrStateConflict", err)
	}
	if _, err := s.Cancel(o.ID); !errors.Is(err, ErrStateConflict) {
		t.Errorf("Cancel after Started: got err %v, want ErrStateConflict", err)
	}
}

func TestLifecycleEndsAreTerminal(t *testing.T) {
	s := New()
	o := mustCreate(t, s, smallOrder(), "")

	for _, want := range []order.Status{order.StatusStarted, order.StatusReadyForPickup, order.StatusDelivered} {
		got, err := s.Advance(o.ID)
		if err != nil {
			t.Fatalf("Advance returned error: %v", err)
		}
		if got.Status != want {
			t.Fatalf("status = %q, want %q", got.Status, want)
		}
	}
	if _, err := s.Advance(o.ID); !errors.Is(err, ErrStateConflict) {
		t.Errorf("Advance after Delivered: got err %v, want ErrStateConflict", err)
	}

	c := mustCreate(t, s, smallOrder(), "")
	if _, err := s.Cancel(c.ID); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	if _, err := s.Cancel(c.ID); !errors.Is(err, ErrStateConflict) {
		t.Errorf("second Cancel: got err %v, want ErrStateConflict", err)
	}
	if _, err := s.Advance(c.ID); !errors.Is(err, ErrStateConflict) {
		t.Errorf("Advance after Cancel: got err %v, want ErrStateConflict", err)
	}
}

func TestUpdateCancelRejectedInAllTerminalStates(t *testing.T) {
	states := []struct {
		name    string
		advance int // how many advances to reach this state
	}{
		{"Started", 1},
		{"Ready for Pickup", 2},
		{"Delivered", 3},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			s := New()
			o := mustCreate(t, s, smallOrder(), "")
			for i := 0; i < st.advance; i++ {
				if _, err := s.Advance(o.ID); err != nil {
					t.Fatalf("advance step %d: %v", i+1, err)
				}
			}
			// Update must fail.
			if _, err := s.Update(o.ID, smallOrder()); !errors.Is(err, ErrStateConflict) {
				t.Errorf("Update in %s: got %v, want ErrStateConflict", st.name, err)
			}
			// Cancel must fail.
			if _, err := s.Cancel(o.ID); !errors.Is(err, ErrStateConflict) {
				t.Errorf("Cancel in %s: got %v, want ErrStateConflict", st.name, err)
			}
		})
	}
	// Cancelled order: Update and Cancel must fail.
	t.Run("Cancelled", func(t *testing.T) {
		s := New()
		o := mustCreate(t, s, smallOrder(), "")
		if _, err := s.Cancel(o.ID); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if _, err := s.Update(o.ID, smallOrder()); !errors.Is(err, ErrStateConflict) {
			t.Errorf("Update after Cancel: got %v, want ErrStateConflict", err)
		}
		if _, err := s.Cancel(o.ID); !errors.Is(err, ErrStateConflict) {
			t.Errorf("second Cancel: got %v, want ErrStateConflict", err)
		}
	})
}

// A failed update must leave the order *entirely* untouched — every field,
// including UpdatedAt, not merely the total and the first item's size.
func TestAtomicValidationFailureLeavesStateUnchanged(t *testing.T) {
	s := New()
	in := order.Input{
		Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Bacon"}}},
		Tip:   pricing.Tip{Mode: pricing.TipAmount, Amount: 100},
	}
	o := mustCreate(t, s, in, "")
	before, ok := s.Get(o.ID)
	if !ok {
		t.Fatal("order should exist")
	}

	for name, bad := range map[string]order.Input{
		"unknown size":      {Items: []pricing.LineItem{{Size: "Huge", Quantity: 1}}},
		"zero quantity":     {Items: []pricing.LineItem{{Size: "Small", Quantity: 0}}},
		"unknown topping":   {Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Gold"}}}},
		"duplicate topping": {Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Bacon", "bacon"}}}},
		"no items":          {Items: nil},
		"unknown promo":     {Items: []pricing.LineItem{{Size: "Small", Quantity: 1}}, PromoCode: "NOPE"},
		"unqualified promo": {Items: []pricing.LineItem{{Size: "Small", Quantity: 1}}, PromoCode: "SAVE5"},
		"bad tip percent":   {Items: []pricing.LineItem{{Size: "Small", Quantity: 1}}, Tip: pricing.Tip{Mode: pricing.TipPercent, Percent: 101}},
		"over max quantity": {Items: []pricing.LineItem{{Size: "Small", Quantity: pricing.MaxQuantity + 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Update(o.ID, bad); err == nil {
				t.Fatalf("Update with %s: want an error, got nil", name)
			}
			got, ok := s.Get(o.ID)
			if !ok {
				t.Fatal("order should still exist")
			}
			// Compare the whole order, not a couple of fields.
			if !reflect.DeepEqual(got, before) {
				t.Errorf("order changed after a failed update:\nbefore %+v\nafter  %+v", before, got)
			}
			if !got.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("UpdatedAt moved on a failed update: %v -> %v", before.UpdatedAt, got.UpdatedAt)
			}
		})
	}

	// A valid update afterwards still works.
	if _, err := s.Update(o.ID, order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 1}}}); err != nil {
		t.Fatalf("valid update after failures: %v", err)
	}
}

// Every accessor must hand back an isolated deep copy, in both directions:
// mutating what a caller received cannot reach the store, and the store
// must not retain a slice the caller passed in.
func TestCopyIsolationForEveryAccessor(t *testing.T) {
	fresh := func(t *testing.T) (*Store, string) {
		t.Helper()
		s := New()
		in := order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Bacon", "Pineapple"}}}}
		return s, mustCreate(t, s, in, "").ID
	}
	sameItems := order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: []string{"Bacon", "Pineapple"}}}}

	accessors := map[string]func(t *testing.T, s *Store, id string) *order.Order{
		"Get": func(t *testing.T, s *Store, id string) *order.Order {
			got, ok := s.Get(id)
			if !ok {
				t.Fatal("Get: order not found")
			}
			return got
		},
		"Update": func(t *testing.T, s *Store, id string) *order.Order {
			got, err := s.Update(id, sameItems)
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			return got
		},
		"Cancel": func(t *testing.T, s *Store, id string) *order.Order {
			got, err := s.Cancel(id)
			if err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			return got
		},
		"Advance": func(t *testing.T, s *Store, id string) *order.Order {
			got, err := s.Advance(id)
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			return got
		},
	}
	for name, get := range accessors {
		t.Run("mutating the result of "+name, func(t *testing.T) {
			s, id := fresh(t)
			returned := get(t, s, id)

			returned.Items[0].Quantity = 999
			returned.Items[0].Size = "Tampered"
			returned.Items[0].Toppings[0] = "Tampered"
			returned.Tip = pricing.Tip{Mode: pricing.TipAmount, Amount: 99999}
			returned.Status = "Tampered"
			returned.Totals.Total = 123456

			stored, ok := s.Get(id)
			if !ok {
				t.Fatal("order should exist")
			}
			if stored.Items[0].Quantity == 999 || stored.Items[0].Size == "Tampered" {
				t.Errorf("stored item changed through the returned copy: %+v", stored.Items[0])
			}
			if stored.Items[0].Toppings[0] == "Tampered" {
				t.Error("stored toppings array is shared with the returned copy")
			}
			if stored.Tip.Amount == 99999 {
				t.Error("stored tip changed through the returned copy")
			}
			if stored.Status == "Tampered" || stored.Totals.Total == 123456 {
				t.Errorf("stored status/totals changed through the returned copy: %+v", stored)
			}
		})
	}

	t.Run("separate reads are independent", func(t *testing.T) {
		s, id := fresh(t)
		first, _ := s.Get(id)
		second, _ := s.Get(id)
		first.Items[0].Toppings[0] = "Tampered"
		if second.Items[0].Toppings[0] == "Tampered" {
			t.Error("two Get results share a toppings array")
		}
	})

	t.Run("caller input is copied on write", func(t *testing.T) {
		s := New()
		toppings := []string{"Bacon"}
		in := order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1, Toppings: toppings}}}
		o := mustCreate(t, s, in, "")
		toppings[0] = "Tampered"
		in.Items[0].Quantity = 999

		stored, _ := s.Get(o.ID)
		if stored.Items[0].Toppings[0] == "Tampered" {
			t.Error("Create retained the caller's toppings array")
		}
		if stored.Items[0].Quantity == 999 {
			t.Error("Create retained the caller's item array")
		}

		upTop := []string{"Pepperoni"}
		upIn := order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 2, Toppings: upTop}}}
		if _, err := s.Update(o.ID, upIn); err != nil {
			t.Fatalf("Update: %v", err)
		}
		upTop[0] = "Tampered"
		upIn.Items[0].Quantity = 777

		stored, _ = s.Get(o.ID)
		if stored.Items[0].Toppings[0] == "Tampered" {
			t.Error("Update retained the caller's toppings array")
		}
		if stored.Items[0].Quantity == 777 {
			t.Error("Update retained the caller's item array")
		}
	})
}

// Concurrency: every caller must get a usable result, and exactly one may
// be told it created the order. Counting distinct IDs alone would pass even
// if most callers had failed outright.
func TestConcurrentIdempotency(t *testing.T) {
	const goroutines = 10
	s := New()
	start := make(chan struct{})

	type result struct {
		id      string
		created bool
		err     error
	}
	results := make(chan result, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			<-start
			o, created, err := s.Create(smallOrder(), "same-key")
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{id: o.ID, created: created}
		}()
	}
	close(start)

	ids := map[string]int{}
	createdCount := 0
	for i := 0; i < goroutines; i++ {
		r := <-results
		// No caller may fail: they all send an identical, valid request.
		if r.err != nil {
			t.Errorf("concurrent Create returned an error: %v", r.err)
			continue
		}
		if r.id == "" {
			t.Error("concurrent Create returned an empty order ID")
			continue
		}
		ids[r.id]++
		if r.created {
			createdCount++
		}
	}
	if len(ids) != 1 {
		t.Fatalf("got %d distinct order IDs, want exactly 1: %v", len(ids), ids)
	}
	// Every caller must have received that one ID.
	for id, n := range ids {
		if n != goroutines {
			t.Errorf("order %s was returned %d times, want %d", id, n, goroutines)
		}
	}
	// Exactly one caller created it; the rest are replays. Without this,
	// every caller could claim creation.
	if createdCount != 1 {
		t.Errorf("%d callers reported created == true, want exactly 1", createdCount)
	}
	if s.Count() != 1 {
		t.Errorf("store holds %d orders, want 1", s.Count())
	}
}

// Concurrent mixed operations on one order must leave it consistent, with
// every caller either succeeding or failing with an explicit conflict.
func TestConcurrentMixedOperationsStayConsistent(t *testing.T) {
	for round := 0; round < 50; round++ {
		s := New()
		o := mustCreate(t, s, smallOrder(), "")

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 4)

		ops := []func() error{
			func() error { _, err := s.Cancel(o.ID); return err },
			func() error { _, err := s.Advance(o.ID); return err },
			func() error { _, err := s.Update(o.ID, smallOrder()); return err },
			func() error {
				if _, ok := s.Get(o.ID); !ok {
					return errors.New("Get could not find the order")
				}
				return nil
			},
		}
		wg.Add(len(ops))
		for _, op := range ops {
			go func(op func() error) {
				defer wg.Done()
				<-start
				errs <- op()
			}(op)
		}
		close(start)
		wg.Wait()
		close(errs)

		for err := range errs {
			// Any failure must be an explicit lifecycle conflict — never a
			// panic, a lost order, or an unrelated error.
			if err != nil && !errors.Is(err, ErrStateConflict) {
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}

		final, ok := s.Get(o.ID)
		if !ok {
			t.Fatalf("round %d: order disappeared", round)
		}
		switch final.Status {
		case order.StatusNotStarted, order.StatusStarted, order.StatusCancelled:
			// All reachable from a concurrent cancel/advance/update.
		default:
			t.Fatalf("round %d: unexpected final status %q", round, final.Status)
		}
		if s.Count() != 1 {
			t.Fatalf("round %d: store holds %d orders, want 1", round, s.Count())
		}
	}
}

func TestAdvanceAll(t *testing.T) {
	s := New()

	// Empty store.
	if n := s.AdvanceAll(); n != 0 {
		t.Errorf("AdvanceAll on empty store returned %d, want 0", n)
	}

	// Three Not Started orders.
	mustCreate(t, s, smallOrder(), "")
	mustCreate(t, s, smallOrder(), "")
	mustCreate(t, s, smallOrder(), "")

	if n := s.AdvanceAll(); n != 3 {
		t.Errorf("AdvanceAll advanced %d, want 3", n)
	}
	if n := s.AdvanceAll(); n != 3 {
		t.Errorf("AdvanceAll second round advanced %d, want 3", n)
	}
	if n := s.AdvanceAll(); n != 3 {
		t.Errorf("AdvanceAll third round advanced %d, want 3", n)
	}
	// Fourth round: all are Delivered (terminal), should advance 0.
	if n := s.AdvanceAll(); n != 0 {
		t.Errorf("AdvanceAll after all delivered: %d, want 0", n)
	}
}

// With orders spread across different statuses, AdvanceAll must move each
// eligible one exactly one step, skip the terminal ones, and report the
// right count. A uniform-state store cannot detect an implementation that
// advances the wrong orders or double-advances.
func TestAdvanceAllWithMixedStates(t *testing.T) {
	s := New()

	notStarted := mustCreate(t, s, smallOrder(), "")

	started := mustCreate(t, s, smallOrder(), "")
	if _, err := s.Advance(started.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	ready := mustCreate(t, s, smallOrder(), "")
	for i := 0; i < 2; i++ {
		if _, err := s.Advance(ready.ID); err != nil {
			t.Fatalf("Advance: %v", err)
		}
	}

	delivered := mustCreate(t, s, smallOrder(), "")
	for i := 0; i < 3; i++ {
		if _, err := s.Advance(delivered.ID); err != nil {
			t.Fatalf("Advance: %v", err)
		}
	}

	cancelled := mustCreate(t, s, smallOrder(), "")
	if _, err := s.Cancel(cancelled.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Three are advanceable; Delivered and Cancelled are terminal.
	if n := s.AdvanceAll(); n != 3 {
		t.Errorf("AdvanceAll advanced %d, want 3", n)
	}

	for _, want := range []struct {
		id     string
		status order.Status
	}{
		{notStarted.ID, order.StatusStarted},
		{started.ID, order.StatusReadyForPickup},
		{ready.ID, order.StatusDelivered},
		{delivered.ID, order.StatusDelivered}, // unchanged
		{cancelled.ID, order.StatusCancelled}, // unchanged
	} {
		got, ok := s.Get(want.id)
		if !ok {
			t.Fatalf("order %s disappeared", want.id)
		}
		if got.Status != want.status {
			t.Errorf("order %s = %q, want %q", want.id, got.Status, want.status)
		}
	}

	// Only the two still-advanceable orders move next time.
	if n := s.AdvanceAll(); n != 2 {
		t.Errorf("second AdvanceAll advanced %d, want 2", n)
	}
	// A cancelled order is never advanced, however many rounds run.
	for i := 0; i < 3; i++ {
		s.AdvanceAll()
	}
	got, _ := s.Get(cancelled.ID)
	if got.Status != order.StatusCancelled {
		t.Errorf("cancelled order became %q", got.Status)
	}
}

func TestCancelDoesNotAffectOtherOrders(t *testing.T) {
	s := New()
	o1 := mustCreate(t, s, smallOrder(), "")
	o2 := mustCreate(t, s, smallOrder(), "")

	if _, err := s.Cancel(o1.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// o2 should be unaffected.
	got, ok := s.Get(o2.ID)
	if !ok {
		t.Fatal("o2 should exist")
	}
	if got.Status != order.StatusNotStarted {
		t.Errorf("o2 status = %q, want %q", got.Status, order.StatusNotStarted)
	}
}

func TestNotFound(t *testing.T) {
	s := New()
	if _, ok := s.Get("ord-9999"); ok {
		t.Error("Get returned a nonexistent order")
	}
	if _, err := s.Update("ord-9999", smallOrder()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update missing order: got %v, want ErrNotFound", err)
	}
	if _, err := s.Cancel("ord-9999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Cancel missing order: got %v, want ErrNotFound", err)
	}
	if _, err := s.Advance("ord-9999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Advance missing order: got %v, want ErrNotFound", err)
	}
}

// The cancel-vs-advance race is the lifecycle's central guarantee: a
// customer cancelling at the exact moment the kitchen starts the order
// must produce exactly one winner and a consistent final state — never
// both succeeding, never a cancelled order that is also Started. The race
// is run many times with both goroutines released simultaneously.
func TestCancelVsAdvanceRaceExactlyOneWinner(t *testing.T) {
	const rounds = 100
	for round := 0; round < rounds; round++ {
		s := New()
		o := mustCreate(t, s, smallOrder(), "")

		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Cancel(o.ID)
			results <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Advance(o.ID)
			results <- err
		}()
		close(start)
		wg.Wait()
		close(results)

		successes := 0
		for err := range results {
			if err == nil {
				successes++
			} else if !errors.Is(err, ErrStateConflict) {
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if successes != 1 {
			t.Fatalf("round %d: %d operations succeeded, want exactly 1", round, successes)
		}
		final, _ := s.Get(o.ID)
		if final.Status != order.StatusCancelled && final.Status != order.StatusStarted {
			t.Fatalf("round %d: final status %q, want Cancelled or Started", round, final.Status)
		}
	}
}
