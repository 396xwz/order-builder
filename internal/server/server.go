// Package server exposes the order store as the order service's HTTP API.
// Handlers are thin: decode, delegate to the store (which enforces every
// lifecycle and pricing rule), and map the outcome to a status code —
// 400 for invalid orders, 404 for unknown IDs, 409 for operations the
// order's current status forbids. All errors are returned as JSON with a
// human-readable message, because the customer on a flaky mobile
// connection is the one who has to act on them.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"pizzabuilder/internal/order"
	"pizzabuilder/internal/pricing"
	"pizzabuilder/internal/store"
)

type handler struct {
	store *store.Store
}

// NewHandler returns the order service's routes.
func NewHandler(st *store.Store) http.Handler {
	h := &handler{store: st}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", h.createOrder)
	mux.HandleFunc("GET /orders/{id}", h.getOrder)
	mux.HandleFunc("PATCH /orders/{id}", h.updateOrder)
	mux.HandleFunc("POST /orders/{id}/cancel", h.cancelOrder)
	mux.HandleFunc("POST /orders/{id}/advance", h.advanceOrder)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// orderView is the wire representation of an order: the order itself plus
// its total formatted for display, so clients never have to format money.
type orderView struct {
	order.Order
	TotalFormatted string `json:"total_formatted"`
}

func view(o *order.Order) orderView {
	return orderView{Order: *o, TotalFormatted: o.Totals.Total.String()}
}

func (h *handler) createOrder(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInput(w, r)
	if !ok {
		return
	}
	o, created, err := h.store.Create(in, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, view(o))
}

func (h *handler) getOrder(w http.ResponseWriter, r *http.Request) {
	o, ok := h.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	writeJSON(w, http.StatusOK, view(o))
}

func (h *handler) updateOrder(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInput(w, r)
	if !ok {
		return
	}
	o, err := h.store.Update(r.PathValue("id"), in)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view(o))
}

func (h *handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	o, err := h.store.Cancel(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view(o))
}

// advanceOrder is the restaurant-side endpoint: each call moves the order
// one step along Not Started -> Started -> Ready for Pickup -> Delivered.
func (h *handler) advanceOrder(w http.ResponseWriter, r *http.Request) {
	o, err := h.store.Advance(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view(o))
}

// decodeInput reads and validates the request body's shape. Semantic
// validation (sizes, toppings, promos, totals) happens in the store via
// the pricing package.
//
// The server enforces a strict JSON shape: no unknown top-level fields,
// no trailing content after the JSON value, and a 1 MiB body limit.
func decodeInput(w http.ResponseWriter, r *http.Request) (order.Input, bool) {
	br := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(br)

	// First decode into a generic map to check for unknown top-level fields.
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return order.Input{}, false
	}
	knownFields := map[string]bool{"items": true, "tip": true, "promo_code": true}
	for key := range raw {
		if !knownFields[key] {
			writeError(w, http.StatusBadRequest, "unknown field "+key)
			return order.Input{}, false
		}
	}

	// Re-marshal into the typed struct to reject mismatched types.
	b, err := json.Marshal(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return order.Input{}, false
	}
	var in order.Input
	if err := json.Unmarshal(b, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return order.Input{}, false
	}

	// Check for trailing content after the JSON value.
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		writeError(w, http.StatusBadRequest, "unexpected content after request body")
		return order.Input{}, false
	} else if !errors.Is(err, io.EOF) {
		// Non-EOF error means non-whitespace trailing bytes.
		writeError(w, http.StatusBadRequest, "unexpected content after request body")
		return order.Input{}, false
	}

	// Validate tip: if mode is empty/none, amount and percent must be zero.
	switch in.Tip.Mode {
	case pricing.TipNone:
		if in.Tip.Amount != 0 {
			writeError(w, http.StatusBadRequest, "tip mode is none but amount_cents is set")
			return order.Input{}, false
		}
		if in.Tip.Percent != 0 {
			writeError(w, http.StatusBadRequest, "tip mode is none but percent is set")
			return order.Input{}, false
		}
	case pricing.TipAmount:
		if in.Tip.Percent != 0 {
			writeError(w, http.StatusBadRequest, "tip mode is amount but percent is also set")
			return order.Input{}, false
		}
	case pricing.TipPercent:
		if in.Tip.Amount != 0 {
			writeError(w, http.StatusBadRequest, "tip mode is percent but amount_cents is also set")
			return order.Input{}, false
		}
	default:
		writeError(w, http.StatusBadRequest, "unknown tip mode")
		return order.Input{}, false
	}

	return in, true
}

// writeStoreError maps store and pricing errors to HTTP status codes.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrStateConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, pricing.ErrPromoNotQualified):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		// Everything else the store returns is a pricing/validation
		// problem with the customer's input.
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
