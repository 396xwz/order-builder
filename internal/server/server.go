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
func decodeInput(w http.ResponseWriter, r *http.Request) (order.Input, bool) {
	var in order.Input
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return in, false
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
