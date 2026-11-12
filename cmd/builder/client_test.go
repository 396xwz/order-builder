package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"pizzabuilder/internal/order"
	"pizzabuilder/internal/pricing"
)

// These tests exercise the real HTTP client path — transport, status
// handling, body decoding, and the submit state machine — against
// in-process servers. They are the regression tests for the
// "response decoding failures are reported as success" and
// "an ambiguous failure must not release the idempotency key" findings.

// newTestSession wires a session to an in-process server and returns the
// session plus its captured output.
func newTestSession(t *testing.T, h http.Handler) (*session, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	out := &bytes.Buffer{}
	return &session{
		client: &apiClient{base: srv.URL, hc: &http.Client{Timeout: 2 * time.Second}},
		out:    out,
		items:  []pricing.LineItem{{Size: "Small", Quantity: 1}},
	}, out
}

// recordingHandler captures requests so tests can assert how many
// submissions were made and with which idempotency keys. Handlers run on
// server goroutines, so the records are mutex-guarded.
type recordingHandler struct {
	mu      sync.Mutex
	keys    []string
	bodies  []string
	respond func(w http.ResponseWriter, r *http.Request, attempt int)
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(r.Body)
	h.mu.Lock()
	h.keys = append(h.keys, r.Header.Get("Idempotency-Key"))
	h.bodies = append(h.bodies, buf.String())
	attempt := len(h.keys)
	h.mu.Unlock()
	h.respond(w, r, attempt)
}

// recorded returns copies of the captured keys and bodies.
func (h *recordingHandler) recorded() (keys, bodies []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.keys...), append([]string(nil), h.bodies...)
}

// ---------------------------------------------------------------------------
// apiClient.call: response body handling
// ---------------------------------------------------------------------------

func TestCallReportsBodyProblems(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "empty body", status: http.StatusCreated, body: ""},
		{name: "malformed JSON", status: http.StatusCreated, body: `{"id":`},
		{name: "truncated JSON", status: http.StatusCreated, body: `{"id":"ord-0001","status":"Not Sta`},
		{name: "html error page", status: http.StatusBadGateway, body: "<html>502 Bad Gateway</html>"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			srv := httptest.NewServer(h)
			defer srv.Close()

			c := &apiClient{base: srv.URL, hc: &http.Client{Timeout: 2 * time.Second}}
			status, resp, err := c.call(http.MethodPost, "/orders", nil, "k")
			if err == nil {
				t.Fatalf("call() returned no error for %s (resp %+v)", tt.name, resp)
			}
			if resp != nil {
				t.Errorf("call() returned a response alongside an error: %+v", resp)
			}
			// The HTTP status must survive so callers can distinguish
			// "201 with an unreadable body" from "never reached".
			if status != tt.status {
				t.Errorf("status = %d, want %d", status, tt.status)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", tt.status)) {
				t.Errorf("error should name the HTTP status, got %v", err)
			}
		})
	}
}

// A body that stops arriving must surface as a timeout so submit's retry
// branch can act on it.
func TestCallClassifiesBodyTimeoutAsTimeout(t *testing.T) {
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.(http.Flusher).Flush() // headers arrive; body never does
		<-release
	})
	srv := httptest.NewServer(h)
	defer func() { close(release); srv.Close() }()

	c := &apiClient{base: srv.URL, hc: &http.Client{Timeout: 50 * time.Millisecond}}
	status, resp, err := c.call(http.MethodPost, "/orders", nil, "k")
	if err == nil {
		t.Fatalf("call() returned no error (resp %+v)", resp)
	}
	if !isTimeout(err) {
		t.Errorf("body-read timeout not classified as a timeout: %v", err)
	}
	if status != http.StatusCreated {
		t.Errorf("status = %d, want 201", status)
	}
}

func TestCallDecodesValidOrder(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ord-0001","status":"Not Started","total_formatted":"$5.33"}`))
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := &apiClient{base: srv.URL, hc: &http.Client{Timeout: 2 * time.Second}}
	status, resp, err := c.call(http.MethodPost, "/orders", &order.Input{}, "k")
	if err != nil {
		t.Fatalf("call() returned error: %v", err)
	}
	if status != http.StatusCreated {
		t.Errorf("status = %d, want 201", status)
	}
	if resp.ID != "ord-0001" || resp.Status != "Not Started" || resp.TotalFormatted != "$5.33" {
		t.Errorf("decoded response = %+v", resp)
	}
}

// ---------------------------------------------------------------------------
// orderResponse validation
// ---------------------------------------------------------------------------

func TestValidateOrder(t *testing.T) {
	for _, tt := range []struct {
		name    string
		resp    *orderResponse
		wantErr bool
	}{
		{"nil response", nil, true},
		{"empty object", &orderResponse{}, true},
		{"missing id", &orderResponse{Status: "Not Started", TotalFormatted: "$5.33"}, true},
		{"missing status", &orderResponse{ID: "ord-1", TotalFormatted: "$5.33"}, true},
		{"missing total", &orderResponse{ID: "ord-1", Status: "Not Started"}, true},
		{"complete", &orderResponse{ID: "ord-1", Status: "Not Started", TotalFormatted: "$5.33"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.resp.validateOrder()
			if (err != nil) != tt.wantErr {
				t.Errorf("validateOrder() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestErrorMessageFallsBackToStatus(t *testing.T) {
	withText := &orderResponse{Error: "unknown promo code"}
	if got := withText.errorMessage(http.StatusBadRequest); got != "unknown promo code" {
		t.Errorf("errorMessage() = %q, want the service's message", got)
	}
	// No JSON error text: the status must still be reported.
	for _, r := range []*orderResponse{nil, {}} {
		got := r.errorMessage(http.StatusBadGateway)
		if !strings.Contains(got, "502") {
			t.Errorf("errorMessage() = %q, want it to mention HTTP 502", got)
		}
	}
}

// ---------------------------------------------------------------------------
// submit: success, validation, and ambiguous-outcome handling
// ---------------------------------------------------------------------------

func TestSubmitSuccess(t *testing.T) {
	h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ord-0001","status":"Not Started","total_formatted":"$5.33"}`))
	}}
	s, out := newTestSession(t, h)

	s.submit()

	if s.orderID != "ord-0001" {
		t.Errorf("orderID = %q, want ord-0001", s.orderID)
	}
	if s.pendingInput != nil {
		t.Error("pendingInput should be cleared after a resolved submission")
	}
	if s.hasPending() {
		t.Error("session should not be pending after success")
	}
	if !strings.Contains(out.String(), "Order ord-0001 submitted") {
		t.Errorf("output = %q", out.String())
	}
}

// An HTTP 201 whose body carries no order must NOT be announced as success
// and must NOT overwrite session state.
func TestSubmitRejectsContentlessSuccessBody(t *testing.T) {
	for _, body := range []string{`{}`, `null`} {
		t.Run(body, func(t *testing.T) {
			h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(body))
			}}
			s, out := newTestSession(t, h)

			s.submit()

			if s.orderID != "" {
				t.Errorf("orderID = %q, want it left empty", s.orderID)
			}
			if !s.hasPending() {
				t.Error("an unusable 201 must leave the submission pending")
			}
			if strings.Contains(out.String(), "submitted. Status:") {
				t.Errorf("success was announced from an empty order body: %q", out.String())
			}
			if !strings.Contains(out.String(), "may or may not have been created") {
				t.Errorf("output should explain the unresolved outcome, got %q", out.String())
			}
		})
	}
}

// A 5xx may follow creation upstream, so the key and snapshot must survive
// and the retry must reuse them instead of creating a second order.
func TestSubmitKeepsKeyOnServerError(t *testing.T) {
	h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
			return
		}
		// The order did exist; the retry replays it.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ord-0001","status":"Not Started","total_formatted":"$5.33"}`))
	}}
	s, out := newTestSession(t, h)

	s.submit()
	firstKey := s.idemKey
	if firstKey == "" {
		t.Fatal("idempotency key was discarded after an ambiguous 503")
	}
	if !s.hasPending() {
		t.Fatal("a 503 must leave the submission pending")
	}
	if s.orderID != "" {
		t.Errorf("orderID = %q, want it left empty", s.orderID)
	}

	out.Reset()
	s.submit()

	if s.idemKey != firstKey {
		t.Errorf("retry used key %q, want the original %q", s.idemKey, firstKey)
	}
	keys, bodies := h.recorded()
	if len(keys) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(keys))
	}
	if keys[0] != keys[1] {
		t.Errorf("retry sent a different key: %q then %q", keys[0], keys[1])
	}
	if bodies[0] != bodies[1] {
		t.Errorf("retry sent different contents:\n%s\n%s", bodies[0], bodies[1])
	}
	if s.orderID != "ord-0001" {
		t.Errorf("orderID = %q, want ord-0001 after the replay", s.orderID)
	}
}

// Likewise for an unreadable 201 body: the retry must reuse the key.
func TestSubmitRetryAfterUnreadableBodyReusesKey(t *testing.T) {
	h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
		if attempt == 1 {
			w.WriteHeader(http.StatusCreated) // created upstream, body lost
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ord-0001","status":"Not Started","total_formatted":"$5.33"}`))
	}}
	s, _ := newTestSession(t, h)

	s.submit()
	if !s.hasPending() || s.idemKey == "" {
		t.Fatal("an empty 201 body must leave the submission pending with its key")
	}
	firstKey := s.idemKey

	s.submit()
	keys, _ := h.recorded()
	if len(keys) != 2 || keys[0] != keys[1] || s.idemKey != firstKey {
		t.Errorf("retry did not reuse the original key: keys=%v session=%q", keys, s.idemKey)
	}
	if s.orderID != "ord-0001" {
		t.Errorf("orderID = %q, want ord-0001", s.orderID)
	}
}

// A 4xx validation rejection is the one case proving nothing was stored, so
// the key is released and editing resumes.
func TestSubmitReleasesKeyOnValidationRejection(t *testing.T) {
	h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"unknown promo code \"NOPE\""}`))
	}}
	s, out := newTestSession(t, h)

	s.submit()

	if s.idemKey != "" || s.pendingInput != nil {
		t.Error("a 4xx rejection should release the key and snapshot")
	}
	if s.hasPending() {
		t.Error("session should not be pending after a definite rejection")
	}
	if !strings.Contains(out.String(), "unknown promo code") {
		t.Errorf("output should carry the service's message, got %q", out.String())
	}
	// Editing must work again.
	out.Reset()
	s.dispatch("add large 1")
	if strings.Contains(out.String(), "outcome is still unknown") {
		t.Errorf("editing should be allowed after a definite rejection, got %q", out.String())
	}
}

// A 409 proves the key IS already used, so it must be kept, and the
// original order ID must be surfaced rather than lost.
func TestSubmitKeepsKeyOnIdempotencyConflict(t *testing.T) {
	h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"idempotency key already used for a different order: key \"k\" created order ord-0007"}`))
	}}
	s, out := newTestSession(t, h)

	s.submit()

	if s.idemKey == "" {
		t.Error("a 409 proves the key was used; it must not be released")
	}
	if !strings.Contains(out.String(), "ord-0007") {
		t.Errorf("output must surface the original order ID, got %q", out.String())
	}
}

// After a timeout the automatic retry fires once with the same key; if that
// also fails the outcome stays pending.
func TestSubmitRetriesOnceOnTimeoutThenStaysPending(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var keys []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		<-release
	})
	srv := httptest.NewServer(h)
	defer func() { close(release); srv.Close() }()

	out := &bytes.Buffer{}
	s := &session{
		client: &apiClient{base: srv.URL, hc: &http.Client{Timeout: 40 * time.Millisecond}},
		out:    out,
		items:  []pricing.LineItem{{Size: "Small", Quantity: 1}},
	}
	s.submit()

	mu.Lock()
	seen := append([]string(nil), keys...)
	mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server saw %d requests, want 2 (one automatic retry)", len(seen))
	}
	if seen[0] != seen[1] {
		t.Errorf("the retry used a different key: %q then %q", seen[0], seen[1])
	}
	if !s.hasPending() {
		t.Error("both attempts failed, so the submission must stay pending")
	}
	if s.orderID != "" {
		t.Errorf("orderID = %q, want it left empty", s.orderID)
	}
}

// Resubmitting after an unresolved outcome must resend the original
// snapshot even though the local order was never allowed to change.
func TestSubmitResendsOriginalSnapshot(t *testing.T) {
	h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"try later"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ord-0001","status":"Not Started","total_formatted":"$5.33"}`))
	}}
	s, _ := newTestSession(t, h)

	s.submit()
	// Attempt an edit; it must be refused while pending.
	s.dispatch("setqty 1 5")
	if s.items[0].Quantity != 1 {
		t.Fatalf("local quantity changed to %d while pending", s.items[0].Quantity)
	}

	s.submit()
	_, bodies := h.recorded()
	if len(bodies) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(bodies))
	}
	if bodies[0] != bodies[1] {
		t.Errorf("resubmission changed the body:\n%s\n%s", bodies[0], bodies[1])
	}
}

// Once an order is known, submit must not start a second creation; the
// customer is directed to update instead.
func TestSubmitAfterKnownOrderDirectsToUpdate(t *testing.T) {
	h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ord-0001","status":"Not Started","total_formatted":"$5.33"}`))
	}}
	s, out := newTestSession(t, h)

	s.submit()
	out.Reset()
	s.submit()

	if keys, _ := h.recorded(); len(keys) != 1 {
		t.Errorf("server saw %d requests, want 1 — no second creation", len(keys))
	}
	if !strings.Contains(out.String(), "already submitted") {
		t.Errorf("output should direct the user to update, got %q", out.String())
	}
}

func TestSubmitEmptyOrderMakesNoRequest(t *testing.T) {
	h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.WriteHeader(http.StatusCreated)
	}}
	s, out := newTestSession(t, h)
	s.items = nil

	s.submit()

	if keys, _ := h.recorded(); len(keys) != 0 {
		t.Errorf("server saw %d requests, want 0", len(keys))
	}
	if !strings.Contains(out.String(), "order is empty") {
		t.Errorf("output = %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// Command arity: invalid arguments must not reach the network
// ---------------------------------------------------------------------------

// Excess arguments must be reported as a usage error and issue no request,
// so `cancel nonexistent-id unexpected` can never cancel the session's own
// order by falling through to s.orderID.
func TestInvalidArityMakesNoRequest(t *testing.T) {
	for _, cmd := range []string{
		"submit unexpected",
		"update unexpected",
		"status id-one id-two",
		"cancel nonexistent-id unexpected",
		"describe unexpected",
	} {
		t.Run(cmd, func(t *testing.T) {
			h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"ord-0001","status":"Cancelled","total_formatted":"$5.33"}`))
			}}
			s, out := newTestSession(t, h)
			s.orderID = "ord-0001"

			s.dispatch(cmd)

			if keys, _ := h.recorded(); len(keys) != 0 {
				t.Errorf("%q issued %d request(s); it must make none", cmd, len(keys))
			}
			if !strings.Contains(out.String(), "Usage:") {
				t.Errorf("%q should report usage, got %q", cmd, out.String())
			}
			// Session state must be untouched.
			if s.orderID != "ord-0001" {
				t.Errorf("orderID changed to %q", s.orderID)
			}
		})
	}
}

// The normal forms still work: the default ID and an explicit ID both
// reach the service, and an explicit ID does not become the session's
// order.
func TestCancelDefaultAndExplicitID(t *testing.T) {
	t.Run("default id", func(t *testing.T) {
		var paths []string
		h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
			paths = append(paths, r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ord-0001","status":"Cancelled","total_formatted":"$5.33"}`))
		}}
		s, out := newTestSession(t, h)
		s.orderID = "ord-0001"

		s.dispatch("cancel")

		if len(paths) != 1 || paths[0] != "/orders/ord-0001/cancel" {
			t.Errorf("requested paths = %v, want one /orders/ord-0001/cancel", paths)
		}
		if !strings.Contains(out.String(), "Order ord-0001 cancelled.") {
			t.Errorf("output = %q", out.String())
		}
	})

	t.Run("explicit id", func(t *testing.T) {
		var paths []string
		h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
			paths = append(paths, r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ord-0042","status":"Cancelled","total_formatted":"$5.33"}`))
		}}
		s, _ := newTestSession(t, h)
		s.orderID = "ord-0001"

		s.dispatch("cancel ord-0042")

		if len(paths) != 1 || paths[0] != "/orders/ord-0042/cancel" {
			t.Errorf("requested paths = %v, want one /orders/ord-0042/cancel", paths)
		}
		// Cancelling someone else's ID must not retarget the session.
		if s.orderID != "ord-0001" {
			t.Errorf("session orderID changed to %q", s.orderID)
		}
	})
}

// With no submitted order and no argument, cancel/status must not guess.
func TestCancelAndStatusWithoutIDMakeNoRequest(t *testing.T) {
	for _, cmd := range []string{"cancel", "status"} {
		t.Run(cmd, func(t *testing.T) {
			h := &recordingHandler{respond: func(w http.ResponseWriter, r *http.Request, attempt int) {
				w.WriteHeader(http.StatusOK)
			}}
			s, out := newTestSession(t, h)

			s.dispatch(cmd)

			if keys, _ := h.recorded(); len(keys) != 0 {
				t.Errorf("%q issued %d request(s) with no known order", cmd, len(keys))
			}
			if !strings.Contains(out.String(), "No order submitted yet") {
				t.Errorf("output = %q", out.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// status / cancel: no false confirmations
// ---------------------------------------------------------------------------

func TestStatusAndCancelRejectContentlessBodies(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	s, out := newTestSession(t, h)
	s.orderID = "ord-0001"

	s.status(nil)
	if !strings.Contains(out.String(), "missing") {
		t.Errorf("status should report the unusable reply, got %q", out.String())
	}

	out.Reset()
	s.cancel(nil)
	if strings.Contains(out.String(), "Order ord-0001 cancelled.") {
		t.Errorf("cancellation must not be confirmed from an empty body: %q", out.String())
	}
	if !strings.Contains(out.String(), "status ord-0001") {
		t.Errorf("output should suggest checking the order, got %q", out.String())
	}
}

func TestNonJSONErrorKeepsHTTPStatusInMessage(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502</html>"))
	})
	s, out := newTestSession(t, h)
	s.orderID = "ord-0001"

	s.status(nil)

	if strings.Contains(out.String(), "Could not reach") {
		t.Errorf("a 502 with a non-JSON body should not be reported as unreachable: %q", out.String())
	}
	if !strings.Contains(out.String(), "502") {
		t.Errorf("output should mention the HTTP status, got %q", out.String())
	}
}
