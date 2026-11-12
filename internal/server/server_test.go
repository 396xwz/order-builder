package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Reusing a key with different contents is a 409 (the request is valid but
// the key is taken), and the message must name the original order so the
// client can still find what it has.
func TestIdempotencyConflictOverHTTP(t *testing.T) {
	srv, st := testServer(t)

	s1, v1 := post(t, srv.URL+"/orders", "same-key", sampleInput())
	if s1 != http.StatusCreated {
		t.Fatalf("first POST status = %d, want 201", s1)
	}

	changed := sampleInput()
	changed.Items[0].Quantity = 5
	status, body := postRaw(t, srv.URL+"/orders", mustJSON(t, changed), "same-key")
	if status != http.StatusConflict {
		t.Errorf("same key with different contents status = %d, want 409. body: %s", status, body)
	}
	if !strings.Contains(body, v1.ID) {
		t.Errorf("409 body must name the original order %s, got: %s", v1.ID, body)
	}
	if st.Count() != 1 {
		t.Errorf("store holds %d orders, want 1", st.Count())
	}

	// Replaying the original request still succeeds with 200.
	s2, v2 := post(t, srv.URL+"/orders", "same-key", sampleInput())
	if s2 != http.StatusOK || v2.ID != v1.ID {
		t.Errorf("replay = (%d, %s), want (200, %s)", s2, v2.ID, v1.ID)
	}
}

// A replay after a PATCH is compared against the original creation request,
// so it keeps working and returns the order's current state.
func TestReplayAfterUpdateOverHTTP(t *testing.T) {
	srv, st := testServer(t)
	_, v := post(t, srv.URL+"/orders", "key-K", sampleInput())

	bigger := order.Input{Items: []pricing.LineItem{{Size: "Large", Quantity: 1}}}
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/orders/"+v.ID, bytes.NewReader([]byte(mustJSON(t, bigger))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200", resp.StatusCode)
	}

	// The original creation request replays successfully.
	status, replay := post(t, srv.URL+"/orders", "key-K", sampleInput())
	if status != http.StatusOK {
		t.Errorf("replay of the original request status = %d, want 200", status)
	}
	if replay.ID != v.ID {
		t.Errorf("replay returned %s, want %s", replay.ID, v.ID)
	}
	if st.Count() != 1 {
		t.Errorf("store holds %d orders, want 1", st.Count())
	}

	// The post-update contents under the same key are a different
	// creation request and are refused.
	conflictStatus, _ := post(t, srv.URL+"/orders", "key-K", bigger)
	if conflictStatus != http.StatusConflict {
		t.Errorf("post-update contents under the same key status = %d, want 409", conflictStatus)
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

// mustJSON marshals a value or fails the test.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

// postRaw sends a raw JSON body and returns the status code and response
// text. An optional idempotency key may be supplied as the final argument.
func postRaw(t *testing.T, url, body string, key ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if len(key) == 1 && key[0] != "" {
		req.Header.Set("Idempotency-Key", key[0])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTrailingContentRejected(t *testing.T) {
	srv, st := testServer(t)
	before := st.Count()

	// Valid input followed by more JSON content.
	status, body := postRaw(t, srv.URL+"/orders", `{"items":[{"size":"Small","quantity":1}]} garbage`)
	if status != http.StatusBadRequest {
		t.Errorf("trailing content status = %d, want 400. body: %s", status, body)
	}
	if st.Count() != before {
		t.Errorf("store changed: %d -> %d", before, st.Count())
	}

	// Two JSON objects.
	status, body = postRaw(t, srv.URL+"/orders", `{"items":[{"size":"Small","quantity":1}]}{"a":1}`)
	if status != http.StatusBadRequest {
		t.Errorf("two JSON objects status = %d, want 400. body: %s", status, body)
	}
	if st.Count() != before {
		t.Errorf("store changed: %d -> %d", before, st.Count())
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	srv, st := testServer(t)
	before := st.Count()

	status, body := postRaw(t, srv.URL+"/orders", `{"items":[{"size":"Small","quantity":1}],"totals":{"subtotal_cents":100}}`)
	if status != http.StatusBadRequest {
		t.Errorf("unknown field status = %d, want 400. body: %s", status, body)
	}
	if !strings.Contains(body, "unknown field") {
		t.Errorf("body should mention unknown field, got: %s", body)
	}
	if st.Count() != before {
		t.Errorf("store changed: %d -> %d", before, st.Count())
	}
}

// patchRaw sends a raw JSON body to PATCH and returns status and text.
func patchRaw(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Money and quantities are integers end to end. A non-integer token must
// be rejected outright, never silently rounded into a valid-looking value
// by passing through a float64 intermediate.
func TestNonIntegerNumbersRejected(t *testing.T) {
	bodies := map[string]string{
		"fractional quantity":         `{"items":[{"size":"Small","quantity":1.00000000000000001}]}`,
		"fractional amount_cents":     `{"items":[{"size":"Small","quantity":1}],"tip":{"mode":"amount","amount_cents":100.000000000000001}}`,
		"plainly fractional quantity": `{"items":[{"size":"Small","quantity":1.5}]}`,
		"fractional percent":          `{"items":[{"size":"Small","quantity":1}],"tip":{"mode":"percent","percent":10.5}}`,
		"exponent quantity":           `{"items":[{"size":"Small","quantity":1e2}]}`,
		"string quantity":             `{"items":[{"size":"Small","quantity":"1"}]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv, st := testServer(t)
			status, resp := postRaw(t, srv.URL+"/orders", body)
			if status != http.StatusBadRequest {
				t.Errorf("POST status = %d, want 400. body: %s", status, resp)
			}
			if st.Count() != 0 {
				t.Errorf("store holds %d orders after a rejected request, want 0", st.Count())
			}
		})
	}
}

// A misspelled nested property must not be silently read as "absent".
func TestNestedUnknownFieldsRejected(t *testing.T) {
	bodies := map[string]string{
		"misspelled tip amount": `{"items":[{"size":"Small","quantity":1}],"tip":{"amount_cent":300}}`,
		"unknown item field":    `{"items":[{"size":"Small","quantity":1,"extra_cheese":true}]}`,
		"unknown tip field":     `{"items":[{"size":"Small","quantity":1}],"tip":{"mode":"amount","amount_cents":300,"currency":"USD"}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv, st := testServer(t)
			status, resp := postRaw(t, srv.URL+"/orders", body)
			if status != http.StatusBadRequest {
				t.Errorf("POST status = %d, want 400. body: %s", status, resp)
			}
			if !strings.Contains(resp, "unknown field") {
				t.Errorf("body should name the unknown field, got: %s", resp)
			}
			if st.Count() != 0 {
				t.Errorf("store holds %d orders, want 0", st.Count())
			}
		})
	}
}

// PATCH shares the same decoder, so it must enforce the same contract and
// leave the existing order untouched when a request is refused.
func TestPatchEnforcesShapeAndLeavesOrderUnchanged(t *testing.T) {
	srv, _ := testServer(t)
	_, v := post(t, srv.URL+"/orders", "", sampleInput())
	before, _ := json.Marshal(v)

	for name, body := range map[string]string{
		"unknown top-level field": `{"items":[{"size":"Large","quantity":1}],"totals":{"total_cents":1}}`,
		"nested unknown field":    `{"items":[{"size":"Large","quantity":1}],"tip":{"amount_cent":300}}`,
		"trailing content":        `{"items":[{"size":"Large","quantity":1}]} garbage`,
		"two JSON objects":        `{"items":[{"size":"Large","quantity":1}]}{"a":1}`,
		"fractional quantity":     `{"items":[{"size":"Large","quantity":1.00000000000000001}]}`,
		"contradictory tip":       `{"items":[{"size":"Large","quantity":1}],"tip":{"mode":"amount","amount_cents":300,"percent":10}}`,
	} {
		t.Run(name, func(t *testing.T) {
			status, resp := patchRaw(t, srv.URL+"/orders/"+v.ID, body)
			if status != http.StatusBadRequest {
				t.Errorf("PATCH status = %d, want 400. body: %s", status, resp)
			}
			// The order must be byte-for-byte what it was.
			getResp, err := http.Get(srv.URL + "/orders/" + v.ID)
			if err != nil {
				t.Fatal(err)
			}
			var got orderView
			_ = json.NewDecoder(getResp.Body).Decode(&got)
			getResp.Body.Close()
			after, _ := json.Marshal(got)
			if string(after) != string(before) {
				t.Errorf("order changed after a rejected PATCH:\nbefore %s\nafter  %s", before, after)
			}
		})
	}

	// A well-formed PATCH still works afterwards.
	status, resp := patchRaw(t, srv.URL+"/orders/"+v.ID, `{"items":[{"size":"Large","quantity":1}]}`)
	if status != http.StatusOK {
		t.Errorf("valid PATCH status = %d, want 200. body: %s", status, resp)
	}
}

// The 1 MiB limit must be enforced while consuming the body, not assumed
// from one successful decode.
func TestBodySizeLimit(t *testing.T) {
	srv, st := testServer(t)

	// A large but legal body is accepted.
	okItem := `{"size":"Small","quantity":1}`
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(okItem)
	}
	b.WriteString(`]}`)
	if status, resp := postRaw(t, srv.URL+"/orders", b.String()); status != http.StatusCreated {
		t.Errorf("large legal body status = %d, want 201. body: %s", status, resp)
	}

	// An oversized body is refused rather than accepted because its
	// leading bytes happened to parse.
	before := st.Count()
	huge := `{"items":[{"size":"Small","quantity":1}],"promo_code":"` + strings.Repeat("A", 2<<20) + `"}`
	status, resp := postRaw(t, srv.URL+"/orders", huge)
	if status != http.StatusRequestEntityTooLarge && status != http.StatusBadRequest {
		t.Errorf("oversized body status = %d, want 413 or 400. body: %s", status, resp)
	}
	if st.Count() != before {
		t.Errorf("oversized body changed the store: %d -> %d", before, st.Count())
	}
}

func TestTipModeValidation(t *testing.T) {
	srv, st := testServer(t)
	before := st.Count()

	// mode=none but amount_cents is set -> 400.
	status, body := postRaw(t, srv.URL+"/orders", `{"items":[{"size":"Small","quantity":1}],"tip":{"mode":"","amount_cents":300}}`)
	if status != http.StatusBadRequest {
		t.Errorf("mode=none with amount status = %d, want 400. body: %s", status, body)
	}
	if !strings.Contains(body, "tip") {
		t.Errorf("body should mention tip, got: %s", body)
	}
	if st.Count() != before {
		t.Errorf("store changed: %d -> %d", before, st.Count())
	}

	// mode=amount with percent set -> 400.
	status, body = postRaw(t, srv.URL+"/orders", `{"items":[{"size":"Small","quantity":1}],"tip":{"mode":"amount","amount_cents":300,"percent":10}}`)
	if status != http.StatusBadRequest {
		t.Errorf("mode=amount with percent status = %d, want 400. body: %s", status, body)
	}
	if st.Count() != before {
		t.Errorf("store changed: %d -> %d", before, st.Count())
	}

	// Legitimate tip should still work.
	status, body = postRaw(t, srv.URL+"/orders", `{"items":[{"size":"Small","quantity":1}],"tip":{"mode":"amount","amount_cents":300}}`)
	if status != http.StatusCreated {
		t.Errorf("valid tip status = %d, want 201. body: %s", status, body)
	}
}
