// The order service: a small HTTP service, run as its own process, that
// receives orders from the order builder, tracks their lifecycle, and is
// the only writer of order state. All pricing is recomputed here via the
// shared pricing package; client-supplied totals are never trusted.
//
// Usage:
//
//	go run ./cmd/orderservice [-addr :8080] [-auto-advance 30s]
//
// The restaurant side advances orders either by calling
// POST /orders/{id}/advance per order, or — with -auto-advance set — on a
// timer that moves every open order one step per interval. Auto-advance
// is OFF by default (0) so the lifecycle can be driven deterministically,
// e.g. by hand or in tests.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"pizzabuilder/internal/server"
	"pizzabuilder/internal/store"
)

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	autoAdvance := flag.Duration("auto-advance", 0,
		"if > 0, advance every open order one lifecycle step per interval (restaurant simulation); 0 = off")
	flag.Parse()

	st := store.New()

	if *autoAdvance > 0 {
		go func() {
			ticker := time.NewTicker(*autoAdvance)
			defer ticker.Stop()
			for range ticker.C {
				if n := st.AdvanceAll(); n > 0 {
					log.Printf("auto-advance: moved %d order(s)", n)
				}
			}
		}()
		log.Printf("auto-advance enabled: one step every %s", *autoAdvance)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.NewHandler(st),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("order service listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
