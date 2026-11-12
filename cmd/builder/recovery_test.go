package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pizzabuilder/internal/pricing"
	"pizzabuilder/internal/server"
	"pizzabuilder/internal/store"
)

// These tests wire the real builder to the real order service through a
// fault-injecting proxy. Everything below the proxy is production code, so
// they verify the end-to-end claim the README makes: an unreliable
// connection may leave the customer unsure, but retrying can never leave
// the restaurant holding two orders.

// faultyProxy forwards to a real service while corrupting or dropping
// replies, simulating a flaky mobile connection. The request always
// reaches the service; only the response is damaged.
type faultyProxy struct {
	target  *httptest.Server
	attempt atomic.Int32
	// damage decides what the client sees for a given attempt. Returning
	// false means "forward the real reply untouched".
	damage func(w http.ResponseWriter, attempt int32) bool
}

func (p *faultyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := p.attempt.Add(1)

	// Always deliver the request to the real service first.
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(r.Body)
	req, err := http.NewRequest(r.Method, p.target.URL+r.URL.Path, bytes.NewReader(body.Bytes()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	upstream := new(bytes.Buffer)
	_, _ = upstream.ReadFrom(resp.Body)

	// The service has now processed it. Decide what the client sees.
	if p.damage(w, n) {
		return
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(upstream.Bytes())
}

// newRecoverySession wires builder -> faulty proxy -> real service.
func newRecoverySession(t *testing.T, damage func(w http.ResponseWriter, attempt int32) bool) (*session, *bytes.Buffer, *store.Store) {
	t.Helper()
	st := store.New()
	svc := httptest.NewServer(server.NewHandler(st))
	t.Cleanup(svc.Close)

	proxy := httptest.NewServer(&faultyProxy{target: svc, damage: damage})
	t.Cleanup(proxy.Close)

	out := &bytes.Buffer{}
	s := &session{
		client: &apiClient{base: proxy.URL, hc: &http.Client{Timeout: 2 * time.Second}},
		out:    out,
		items:  []pricing.LineItem{{Size: "Medium", Quantity: 2, Toppings: []string{"Bacon"}}},
		tip:    pricing.Tip{Mode: pricing.TipAmount, Amount: 300},
	}
	return s, out, st
}

// firstReply damages only the first attempt; the retry sees the true reply.
func firstReply(write func(w http.ResponseWriter)) func(http.ResponseWriter, int32) bool {
	return func(w http.ResponseWriter, attempt int32) bool {
		if attempt == 1 {
			write(w)
			return true
		}
		return false
	}
}

// The central recovery guarantee: whatever the network did to the reply,
// retrying leaves exactly ONE order in the restaurant's system, and the
// customer ends up holding that order's real ID and total.
func TestRetryRecoversExactlyOneOrder(t *testing.T) {
	for _, tt := range []struct {
		name   string
		damage func(http.ResponseWriter, int32) bool
	}{
		{"first reply body is lost", firstReply(func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated) // created, body never arrives
		})},
		{"first reply is truncated JSON", firstReply(func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ord-00`))
		})},
		{"first reply is a gateway error", firstReply(func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
		})},
		{"first reply is 408 after the order was stored", firstReply(func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusRequestTimeout)
			_, _ = w.Write([]byte(`{"error":"request timed out"}`))
		})},
		{"first reply is a contentless 200", firstReply(func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		})},
		{"first reply is 202 Accepted", firstReply(func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"ord-0001","status":"Not Started","total_formatted":"$16.29"}`))
		})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, out, st := newRecoverySession(t, tt.damage)

			// First attempt: the service stores the order, the reply is lost.
			s.submit()

			if st.Count() != 1 {
				t.Fatalf("service holds %d orders after the first attempt, want 1", st.Count())
			}
			if s.orderID != "" {
				t.Errorf("builder claimed order %q from a damaged reply", s.orderID)
			}
			if !s.hasPending() {
				t.Fatal("the outcome is unknown, so the submission must stay pending")
			}

			// Retrying must recover the SAME order, not create a second.
			out.Reset()
			s.submit()

			if st.Count() != 1 {
				t.Errorf("service holds %d orders after the retry, want 1 — a duplicate was created", st.Count())
			}
			if s.orderID == "" {
				t.Fatalf("the retry did not resolve the order; output: %q", out.String())
			}
			// The ID the customer now holds must be the order that exists.
			stored, ok := st.Get(s.orderID)
			if !ok {
				t.Fatalf("builder holds %q, which is not in the service", s.orderID)
			}
			if s.hasPending() {
				t.Error("the submission should be resolved after a successful retry")
			}
			// And the total shown must be the total actually charged.
			if !strings.Contains(out.String(), stored.Totals.Total.String()) {
				t.Errorf("output %q does not report the charged total %s", out.String(), stored.Totals.Total)
			}
		})
	}
}

// A genuine validation rejection is not a recovery case: nothing is stored,
// the key is released, and a corrected resubmission creates exactly one
// order rather than being blocked or duplicated.
func TestRejectionThenCorrectedResubmission(t *testing.T) {
	// Rewrite the first request so the service sees an order it must
	// refuse. The builder pre-checks its own state, so a server-side
	// rejection has to come from something the client believed was valid.
	var rewritten atomic.Bool
	st := store.New()
	svc := httptest.NewServer(server.NewHandler(st))
	t.Cleanup(svc.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		payload := body.Bytes()
		if r.Method == http.MethodPost && !rewritten.Swap(true) {
			payload = []byte(`{"items":[{"size":"Small","quantity":1}],"promo_code":"SAVE5"}`)
		}
		req, _ := http.NewRequest(r.Method, svc.URL+r.URL.Path, bytes.NewReader(payload))
		for k, vs := range r.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Content-Length", "")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out.Bytes())
	}))
	t.Cleanup(proxy.Close)

	out := &bytes.Buffer{}
	s := &session{
		client: &apiClient{base: proxy.URL, hc: &http.Client{Timeout: 2 * time.Second}},
		out:    out,
		items:  []pricing.LineItem{{Size: "Medium", Quantity: 2}},
	}

	s.submit()

	if st.Count() != 0 {
		t.Fatalf("service stored %d orders for a rejected request, want 0", st.Count())
	}
	if s.idemKey != "" || s.hasPending() {
		t.Error("a definite rejection should release the key and clear pending state")
	}
	if !strings.Contains(out.String(), "No order was created") {
		t.Errorf("output should confirm nothing was created, got %q", out.String())
	}

	// Resubmit now that the connection is behaving: exactly one order.
	out.Reset()
	s.submit()

	if st.Count() != 1 {
		t.Errorf("service holds %d orders after the corrected submission, want 1", st.Count())
	}
	if s.orderID == "" {
		t.Errorf("corrected submission did not resolve; output: %q", out.String())
	}
}

// Editing is refused while an outcome is unknown, so a retry cannot send
// different contents under the same key — verified against the real service
// by comparing what it stored with what the builder still holds locally.
func TestPendingSubmissionPreservesContentsEndToEnd(t *testing.T) {
	s, _, st := newRecoverySession(t, firstReply(func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusCreated) // stored upstream, reply lost
	}))

	s.submit()
	if !s.hasPending() {
		t.Fatal("expected a pending submission")
	}

	// Every edit must be refused while pending.
	for _, cmd := range []string{"add large 3", "setqty 1 9", "tip percent 25", "promo ILOVEPIZZA", "remove 1"} {
		s.dispatch(cmd)
	}
	if len(s.items) != 1 || s.items[0].Quantity != 2 || s.items[0].Size != "Medium" {
		t.Errorf("local items changed while pending: %+v", s.items)
	}
	if s.tip.Mode != pricing.TipAmount || s.tip.Amount != 300 {
		t.Errorf("local tip changed while pending: %+v", s.tip)
	}
	if s.promo != "" {
		t.Errorf("local promo changed while pending: %q", s.promo)
	}

	s.submit()
	if st.Count() != 1 {
		t.Fatalf("service holds %d orders, want 1", st.Count())
	}
	stored, ok := st.Get(s.orderID)
	if !ok {
		t.Fatalf("order %q not found in the service", s.orderID)
	}
	// What the service stored must match what the builder still shows.
	if len(stored.Items) != 1 || stored.Items[0].Quantity != 2 || stored.Items[0].Size != "Medium" {
		t.Errorf("stored items %+v do not match the local order %+v", stored.Items, s.items)
	}
	if stored.Tip != s.tip {
		t.Errorf("stored tip %+v does not match the local tip %+v", stored.Tip, s.tip)
	}
	// The charged total must equal what the shared pricing package computes.
	want, err := pricing.Price(s.items, s.tip, s.promo)
	if err != nil {
		t.Fatalf("pricing.Price: %v", err)
	}
	if stored.Totals != want {
		t.Errorf("service totals %+v != locally computed %+v", stored.Totals, want)
	}
}
