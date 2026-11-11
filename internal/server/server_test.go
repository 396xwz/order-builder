package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"pizzabuilder/internal/order"
	"pizzabuilder/internal/pricing"
	"pizzabuilder/internal/store"
)

func testServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st := store.New()
	srv := httptest.NewServer(NewHandler(st))
	t.Cleanup(srv.Close)
	return srv, st
}

func post(t *testing.T, url, key string, in order.Input) (int, orderView) {
	t.Helper()
	body, _ := json.Marshal(in)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var v orderView
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func sampleInput() order.Input {
	return order.Input{
		Items: []pricing.LineItem{
			{Size: "Medium", Quantity: 2, Toppings: []string{"Extra Cheese"}},
			{Size: "Small", Quantity: 1},
		},
		Tip:       pricing.Tip{Mode: pricing.TipAmount, Amount: 300},
		PromoCode: "SAVE5",
	}
}

// The builder/service agreement: the totals the service charges for an
// order are exactly what the shared pricing package computes locally for
// the same input — the customer is never charged a different total than
// the one they watched while building.
func TestSubmitTotalsAgreeWithPricingPackage(t *testing.T) {
	srv, _ := testServer(t)
	in := sampleInput()

	want, err := pricing.Price(in.Items, in.Tip, in.PromoCode)
	if err != nil {
		t.Fatalf("pricing.Price: %v", err)
	}
	status, v := post(t, srv.URL+"/orders", "k1", in)
	if status != http.StatusCreated {
		t.Fatalf("POST /orders status = %d, want 201", status)
	}
	if v.Totals != want {
		t.Errorf("service totals %+v != locally computed %+v", v.Totals, want)
	}
	if v.Status != order.StatusNotStarted {
		t.Errorf("new order status = %q, want %q", v.Status, order.StatusNotStarted)
	}

	// And a fresh read returns the same totals.
	resp, err := http.Get(srv.URL + "/orders/" + v.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got orderView
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Totals != want {
		t.Errorf("GET totals %+v != %+v", got.Totals, want)
	}
}

func TestIdempotentReplayOverHTTP(t *testing.T) {
	srv, st := testServer(t)
	in := sampleInput()

	s1, v1 := post(t, srv.URL+"/orders", "same-key", in)
	s2, v2 := post(t, srv.URL+"/orders", "same-key", in)
	if s1 != http.StatusCreated || s2 != http.StatusOK {
		t.Errorf("statuses = %d, %d; want 201 then 200 (replay)", s1, s2)
	}
	if v1.ID != v2.ID {
		t.Errorf("replay returned %s, want original %s", v2.ID, v1.ID)
	}
	if st.Count() != 1 {
		t.Errorf("store holds %d orders, want 1", st.Count())
	}
}

func TestLifecycleConflictsOverHTTP(t *testing.T) {
	srv, _ := testServer(t)
	_, v := post(t, srv.URL+"/orders", "", sampleInput())
	advance := fmt.Sprintf("%s/orders/%s/advance", srv.URL, v.ID)
	cancel := fmt.Sprintf("%s/orders/%s/cancel", srv.URL, v.ID)

	resp, _ := http.Post(advance, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("advance status = %d, want 200", resp.StatusCode)
	}

	// Update and cancel are now illegal: 409 with a readable message.
	body, _ := json.Marshal(sampleInput())
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/orders/"+v.ID, bytes.NewReader(body))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("PATCH after Started status = %d, want 409", resp.StatusCode)
	}
	resp, _ = http.Post(cancel, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cancel after Started status = %d, want 409", resp.StatusCode)
	}
}

func TestBadRequests(t *testing.T) {
	srv, _ := testServer(t)

	// Unknown promo: 400.
	bad := sampleInput()
	bad.PromoCode = "NOPE"
	if status, _ := post(t, srv.URL+"/orders", "", bad); status != http.StatusBadRequest {
		t.Errorf("unknown promo status = %d, want 400", status)
	}
	// SAVE5 on a small order does not qualify: 400, not a silent no-op.
	small := order.Input{Items: []pricing.LineItem{{Size: "Small", Quantity: 1}}, PromoCode: "SAVE5"}
	if status, _ := post(t, srv.URL+"/orders", "", small); status != http.StatusBadRequest {
		t.Errorf("unqualified SAVE5 status = %d, want 400", status)
	}
	// Unknown order: 404.
	resp, _ := http.Get(srv.URL + "/orders/ord-9999")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET missing order status = %d, want 404", resp.StatusCode)
	}
}
