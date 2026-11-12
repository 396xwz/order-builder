package store

import (
	"errors"
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
	if err == nil {
		t.Fatal("expected error for different body with same key")
	}
	if created {
		t.Fatal("created should be false for rejected replay")
	}
	if !strings.Contains(err.Error(), o.ID) {
		t.Errorf("error should mention the original order ID %q, got %v", o.ID, err)
	}
	if s.Count() != 1 {
		t.Errorf("store holds %d orders, want 1", s.Count())
	}
}

func TestInputsEqual(t *testing.T) {
	in := smallOrder()
	o := &order.Order{
		Items:     []pricing.LineItem{{Size: "Small", Quantity: 1}},
		PromoCode: "",
	}
	if !inputsEqual(in, o) {
		t.Error("identical inputs should be equal")
	}
	// Different items.
	in2 := order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 1}}}
	if inputsEqual(in2, o) {
		t.Error("different items should not be equal")
	}
	// Different promo.
	o.PromoCode = "SAVE5"
	if inputsEqual(in, o) {
		t.Error("different promo should not be equal")
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

func TestAtomicValidationFailureLeavesStateUnchanged(t *testing.T) {
	s := New()
	in := order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1}}, Tip: pricing.Tip{Mode: pricing.TipAmount, Amount: 100}}
	o := mustCreate(t, s, in, "")
	original := o.Totals.Total

	// Try an invalid update (unknown size).
	bad := order.Input{Items: []pricing.LineItem{{Size: "Huge", Quantity: 1}}}
	_, err := s.Update(o.ID, bad)
	if err == nil {
		t.Fatal("expected error for invalid update")
	}

	// Re-read and verify unchanged.
	got, ok := s.Get(o.ID)
	if !ok {
		t.Fatal("order should still exist")
	}
	if got.Totals.Total != original {
		t.Errorf("total changed from %s to %s after failed update", original, got.Totals.Total)
	}
	if len(got.Items) != 1 || got.Items[0].Size != "Small" {
		t.Errorf("items changed after failed update: %+v", got.Items)
	}
}

func TestStoreOwnershipPreventsExternalMutation(t *testing.T) {
	s := New()
	o := mustCreate(t, s, smallOrder(), "")
	// Mutate the returned copy.
	o.Items[0].Quantity = 999
	o.Tip.Amount = 99999

	// The stored order must be unchanged.
	got, ok := s.Get(o.ID)
	if !ok {
		t.Fatal("order should exist")
	}
	if got.Items[0].Quantity == 999 {
		t.Error("stored quantity changed after external mutation")
	}
	if got.Tip.Amount == 99999 {
		t.Error("stored tip changed after external mutation")
	}
}

func TestConcurrentIdempotency(t *testing.T) {
	const goroutines = 10
	s := New()
	start := make(chan struct{})
	results := make(chan string, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			<-start
			o, _, err := s.Create(smallOrder(), "same-key")
			if err != nil {
				results <- ""
				return
			}
			results <- o.ID
		}()
	}
	close(start)

	ids := map[string]int{}
	for i := 0; i < goroutines; i++ {
		id := <-results
		if id != "" {
			ids[id]++
		}
	}
	if len(ids) != 1 {
		t.Errorf("got %d distinct order IDs, want exactly 1: %v", len(ids), ids)
	}
	if s.Count() != 1 {
		t.Errorf("store holds %d orders, want 1", s.Count())
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
