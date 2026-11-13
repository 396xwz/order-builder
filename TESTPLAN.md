# TESTPLAN — Pizza-Driven Development (order-builder)

Manual / scripted end-to-end verification plan for the take-home, run against
the real service and builder — not the unit tests. Every expected value below
was hand-computed from the spec and the menu data in `internal/pricing`, so
this plan is an independent check on the implementation, not a restatement of
what the code does.

## Execution record (this version)

- **Commit under test:**  HEAD as fetched Thu Nov 12, 2026.
- **Executed:** Thu Nov 12, 2026, afternoon PT, by Muse — live service
  (`go run ./cmd/orderservice` binary), scripted builder sessions (piped
  stdin), curl battery, concurrent-race harness, and a fault-injection
  proxy for G4. Full unit suite also run: `go test ./...` — all packages
  pass (`cmd/builder`, `internal/order`, `internal/pricing`,
  `internal/server`, `internal/store`; `cmd/orderservice` has no test
  files by design).
- **How to re-run:** the executor records actual output verbatim and marks
  Pass/Fail — it does NOT edit code to make a case pass. Failures are
  reported as failures. A case passes only if the actual output matches
  the expected output exactly (amounts to the cent).

**Setup**

```sh
go run ./cmd/orderservice            # terminal 1, listens on :8080
go run ./cmd/builder                 # terminal 2, interactive CLI
# API checks use curl against http://localhost:8080
```

---

## A. Menu coverage (builder `menu` + `add`/`describe`)

Topping prices, derived from the category percentages (basic 8%, premium 11%
of base). All land on exact cents for every size, so no rounding is involved.

| Size | Base | Basic topping | Premium topping |
| --- | --- | --- | --- |
| Small | $5.00 | $0.40 | $0.55 |
| Medium | $7.00 | $0.56 | $0.77 |
| Large | $12.00 | $0.96 | $1.32 |

- **A1.** `menu` shows all three sizes with the base prices above, all 12
  toppings in the right category (basic: Pepperoni, Sausage, Extra Cheese,
  Onions, Black Olives, Green Peppers; premium: Bacon, Chicken, Garlic,
  Artichoke Hearts, Sun-Dried Tomatoes, Pineapple), and both promos with
  their descriptions.
- **A2.** Add one of each size plain, `describe`: subtotal $5.00 + $7.00 +
  $12.00 = **$24.00**, tax 6.5% = **$1.56**, total **$25.56** (no tip/promo).
- **A3.** One Large with all 6 basic toppings: $12.00 + 6 × $0.96 = **$17.76**
  subtotal.
- **A4.** One Small with all 6 premium toppings: $5.00 + 6 × $0.55 = **$8.30**
  subtotal.
- **A5.** One Medium, one basic + one premium topping: $7.00 + $0.56 + $0.77
  = **$8.33** subtotal; tax $0.54 (6.5% of $8.33 = $0.54145 → $0.54);
  total **$8.87**.
- **A6.** Quantity: `add large 3 bacon` → 3 × ($12.00 + $1.32) = **$39.96**
  subtotal.
- **A7.** Case/underscore tolerance: `add LARGE 1 extra_cheese` and
  `add large 1 EXTRA_CHEESE` both work and price identically.

## B. Pricing arithmetic (order of operations)

Order of operations: subtotal → promo discount (pre-tax) → tax 6.5% of the
post-discount subtotal → tip → total. Rounding is round-half-up, applied at
the order level in exactly three places (percent discount, tax, percent tip).

- **B1 (README example).** `add large 1 bacon,pineapple` → subtotal **$14.64**,
  tax **$0.95**, total **$15.59** (no tip/promo).
- **B2 (full stack).** Build to subtotal $24.64, apply ILOVEPIZZA, tip 20%:
  discount **−$2.46** (10% of $24.64 = $2.464 → $2.46), taxable $22.18, tax
  **$1.44** (6.5% of $22.18 = $1.4417 → $1.44), post-tax $23.62, tip **$4.72**
  (20% of $23.62 = $4.724 → $4.72), total **$28.34**.
- **B3 (tip base).** Confirm a percent tip is computed on the post-discount,
  post-tax total (B2's $4.72), not on the pre-discount subtotal (20% of
  $24.64 would be $4.93 — that would be wrong).
- **B4 (README API example).** POST 2 Medium with Extra Cheese + 1 Small,
  $3.00 amount tip, SAVE5: subtotal **$20.12**, discount **−$5.00**, taxable
  $15.12, tax **$0.98**, tip $3.00, total **$19.10**.

## C. Promo codes

- **C1.** ILOVEPIZZA on a $5.00 Small: discount $0.50, taxable $4.50, tax
  $0.29, total **$4.79**.
- **C2 (SAVE5 boundary, exact).** Order totalling exactly $20.00 pre-tax
  (e.g. 4 plain Smalls): SAVE5 is **rejected** — "over $20" means $20.00
  itself does not qualify. Builder reports the reason; API returns 400.
- **C3 (SAVE5 boundary, just over).** Order totalling $20.01+ pre-tax (e.g.
  B4's $20.12): SAVE5 applies, discount exactly **$5.00**.
- **C4 (mutual exclusion).** Apply ILOVEPIZZA, then SAVE5 → second promo
  refused. Reverse order (SAVE5 first, then ILOVEPIZZA) → likewise refused.
- **C5.** Unknown code (e.g. `promo PIZZA20`) → rejected with a clear error,
  no discount applied.
- **C6.** `promo clear` removes the promo; totals return to the no-promo
  values.

## D. Tips

- **D1.** `tip amount 3.50` adds exactly $3.50 to the total.
- **D2.** `tip percent 20` on a 2-Small order (subtotal $10.00, post-tax
  bill $10.65) adds **$2.13** (20% of $10.65 = $2.13), total $12.78.
- **D3.** `tip percent 0` and `tip percent 100` are accepted (100% doubles
  the bill: Small plain goes $5.33 → $10.66). `tip percent 101` and
  `tip percent -5` are rejected.
- **D4.** `tip clear` removes the tip; total drops back to the pre-tip value.
- **D5.** A dollar tip with more than two decimal places
  (`tip amount 3.005`) is rejected; `3.50` and `$3.50` both parse.

## E. Builder editing and input validation

- **E1.** `remove 1` removes the line; running total updates.
- **E2.** `setqty 1 3` changes the line quantity; total recomputes.
- **E3.** `settoppings 1 none` clears toppings; `settoppings 1 bacon` replaces
  them; total recomputes.
- **E4.** Invalid size (`add huge 1`) → clear error, order unchanged.
- **E5.** Unknown topping (`add large 1 anchovies`) → clear error, order
  unchanged.
- **E6.** Invalid quantity (`add large 0`, `add large -1`, `add large abc`)
  → rejected.
- **E7.** Unknown command (`frobnicate`) → "Unknown command" hint, session
  continues.
- **E8.** `describe` on an empty order shows an empty order, not an error.
- **E9 (pending block).** After a submit ends with an unknown outcome,
  `add`/`remove`/`setqty`/`settoppings` are blocked with "That change was
  not applied: the previous submission's outcome is still unknown" until
  the submission resolves via retry.
- **E10 (dollar parsing).** Signed fractional parts are rejected:
  `tip amount 1.-1` and `tip amount 1.+1` → "has invalid digits in the
  fractional part".
- **E11 (command arity).** `status` with 2+ args, `update` with any args,
  `cancel` with 2+ args, `describe` with any args, and `submit` with any
  args → usage error and NO network call. In particular, `submit extra`
  prints "Usage: submit (no arguments). Nothing was sent." and creates no
  order, and `cancel <id> <extra>` must not fall back to cancelling the
  session's submitted order.
- **E12 (monetary bound).** An absurd dollar amount is rejected, not
  wrapped: `tip amount 184467440737095517` → "exceeds the maximum amount
  of $1000000000.00" (previously this wrapped and displayed as $0.84).

## F. Order lifecycle (API, curl)

- **F1.** `POST /orders` with a valid body → **201**, status `Not Started`,
  totals recomputed server-side. There is no totals field a client can
  send: a client-supplied `totals`/`status`/`id` is **rejected with 400**
  (unknown field), not ignored — see J1.
- **F2.** `GET /orders/{id}` → **200** with the order and its status.
- **F3.** `PATCH /orders/{id}` while Not Started → **200**, items/tip/promo
  replaced wholesale, totals recomputed.
- **F4.** `POST /orders/{id}/advance` → **200**, status becomes `Started`.
- **F5.** After Started: `PATCH` → **409**; `POST /cancel` → **409**.
- **F6.** Advance again → `Ready for Pickup`; again → `Delivered`.
- **F7.** Advance past Delivered → **409**; cancel Delivered → **409**;
  update Delivered → **409**.
- **F8.** Cancel while Not Started → **200**, status `Cancelled`; then
  advance/update/cancel on it → **409** each.
- **F9.** Unknown order id on GET/PATCH/cancel/advance → **404**.
- **F10.** `-auto-advance` moves open orders one step per interval without
  manual advance calls (executed with a 2s interval: an order reached
  Delivered within 5s).

## G. Idempotency and the unreliable network (requirement 5)

- **G1.** `POST /orders` with `Idempotency-Key: k1` → 201. Repeat the exact
  request with `k1` → **200**, the *original* order returned, no duplicate
  created (verify with GET: only one order exists).
- **G2.** Same key `k1` with a *different* body → **409**
  (`ErrIdempotencyConflict`), and the error names the original order id:
  "idempotency key already used for a different order: key "k1" created
  order ord-0003; retry that exact request or use PATCH ... to change it".
  (Earlier behavior returned a bare 400; the 409 names the order so the
  client is not stranded.)
- **G3.** A second `POST /orders` with no key → always creates a new order.
- **G4 (lost-response recovery — fault injection).** A stopped service does
  NOT test this: it tests connectivity. The real check needs the request
  to reach the service, the order to be created, and only the *reply* to
  be lost. Procedure: run the service behind a small proxy that forwards
  the first N `POST /orders` requests to the real service, reads the
  backend response (proving the order was created), and then closes the
  client connection without writing anything. Drive the builder through
  the proxy (`-service <proxy> -timeout 3s`):
  1. `submit` → the builder reports the submission did not complete
     ("Post ...: EOF"), says the order may or may not have been created,
     keeps the idempotency key and snapshot, and pauses editing.
  2. `submit` again → "Retrying the previous submission — its outcome is
     still unknown. The original contents and idempotency key are reused,
     so this cannot create a duplicate." Same unresolved outcome while
     replies are still being dropped.
  3. Once the proxy passes replies through, `submit` again → "The service
     already had this order (idempotent replay) — no duplicate was
     created." and the original order id and total.
  4. Server-side verification: exactly ONE order was created across all
     attempts (check the next order id does not exist).
  Automated coverage of the same property lives in the repo:
  `cmd/builder/recovery_test.go` (`TestRetryRecoversExactlyOneOrder`)
  drives the real builder against the real service through a faulty proxy
  for six failure modes (lost body, truncated JSON, 502, a 408 issued
  after the order was stored, a contentless 200, and a 202), asserting
  exactly one order in the store each time.
- **G5 (decode fix).** A truncated, empty, or contentless 2xx response
  body is reported as an error by the builder, not as a successful
  submission with an empty status. (This is the bug the review caught:
  the original client discarded the JSON decode error.) Covered by unit
  tests `TestCallReportsBodyProblems`,
  `TestSubmitRejectsContentlessSuccessBody`, and
  `TestStatusAndCancelRejectContentlessBodies` — all passing
  A manual proxy variant was not run in this pass.
- **G6 (replay after PATCH).** Replay comparison is against the ORIGINAL
  creation request, not the order's current contents: create with key `k`
  (Small ×1), PATCH the order to different contents, then re-POST the
  original body with `k` → **200**, returning the order (in its current,
  patched state). Re-POST the *patched* contents with `k` → **409**,
  because they differ from what `k` originally created.

## H. Overflow guards

- **H1.** Quantity above 1,000,000 for one line item is rejected (400,
  "quantity 1000001 exceeds maximum of 1000000 per line item").
- **H2.** Monetary maximums are enforced, not wrapped: an order whose
  subtotal exceeds $1,000,000,000.00 → 400 ("subtotal $1200000000.00
  exceeds maximum of $1000000000.00"); an order whose *total* exceeds the
  maximum (e.g. 83 lines × 1M Large: subtotal $996,000,000, total
  $1,060,740,000) → 400 ("order total $1060740000.00 exceeds the maximum
  of $1000000000.00"). A large-but-legal order (2 lines × 1M Large,
  total $25,560,000.00) succeeds with 201.

## I. Error handling

- **I1.** `POST /orders` with malformed JSON → **400** with a readable error.
- **I2.** `POST /orders` with an empty body → **400** ("invalid request
  body: EOF").
- **I3.** `POST /orders` with an invalid order (unknown size, unknown
  topping, zero quantity) → **400**.

## J. Edge cases and adversarial input ("how people try to hack it")

Nothing exotic — the realistic ways a hostile or sloppy client pokes at a
small service. All expected results below were executed live in this pass

**Trusting the client (totals / identity / status):**

- **J1.** POST an order with a client-supplied `totals` object
  (`{"total_cents": 1}`), a `status` of `"Delivered"`, or an `id` of
  `"ord-9999"` → **400**, `invalid request body: json: unknown field
  "totals"` (or the field's name). The service decodes directly into the
  typed struct with `DisallowUnknownFields` at **every** level, so nested
  unknowns are rejected too: `{"tip": {"amount_cent": 300}}` → 400, and
  an unknown item field (`"crust"`) → 400. A rejected request creates
  nothing (verified: the next order id does not exist afterwards).
- **J2.** PATCH an order and include `status`/`totals` in the body → **400**
  for the same reason; the stored order is byte-for-byte unchanged
  (verified by re-GET before and after).

**Validation edges:**

- **J3.** Duplicate topping on one pizza (`["Bacon","Bacon"]`) → **400**,
  "topping Bacon is listed twice on the same pizza" (not charged twice, not
  silently deduped).
- **J4.** Empty items array (`"items": []`) → **400**, "order must contain
  at least one line item". Missing `items` field entirely → same 400.
- **J5.** Non-integer numbers are rejected, never coerced: quantity `1.5`,
  `1e2`, `"1"` (quoted), `1.00000000000000001`, and
  `amount_cents: 100.000000000000001` → **400** each. (Decoding straight
  into the typed struct preserves integer tokens; an earlier map-based
  decoder re-rendered these as valid integers via float64.)
- **J6.** Negative tip amount (`"tip": {"mode": "amount", "amount_cents": -500}`)
  → **400**. Tip percent `101` / `-5` → **400** (already D3; re-check via API).
- **J7.** Unknown tip mode (`"mode": "crypto"`) → **400** ("unknown tip
  mode").
- **J8.** Promo code case/whitespace: `"ilovepizza"`, `" ILOVEPIZZA "` →
  accepted and canonicalized to `ILOVEPIZZA` in the stored order (discount
  applies). Unknown code with odd casing (`"save99"`) → **400**.
- **J9.** Quantity `0` or negative via API → **400**; quantity `1000001`
  → **400** (MaxQuantity, see H1).

**HTTP-level edges:**

- **J10.** Wrong method on a known route (`DELETE /orders`,
  `GET /orders/{id}/cancel`) → **405**, not a routed handler.
- **J11.** Unknown route (`GET /nope`) → **404**; trailing slash
  (`POST /orders/`) → **404**.
- **J12.** Oversized bodies — two distinct behaviors, both verified:
  (a) a single JSON value larger than the 1 MB cap (2 MB body) → **400**,
  "invalid request body: http: request body too large" (the first decode
  fails before any value is read); (b) a valid small JSON value followed
  by more than 1 MB of trailing content → **413**, "request body too
  large" (the trailing-content check is where the 413 is raised). Either
  way the payload cannot exhaust memory.
- **J13.** Path-traversal-style order id (`GET /orders/..%2F..%2Fetc`,
  `GET /orders/ord-0001/extra`) → **404**, no filesystem or routing
  weirdness — ids are map keys, never paths.
- **J14.** Missing `Content-Type: application/json` header → request still
  processed (the service decodes the body regardless; no content-type
  gate). This is a deliberate leniency, not a bug — note it, don't fail it.
- **J15.** `GET /healthz` → **200** `{"status": "ok"}` (liveness check).
- **J16 (strict JSON).** Trailing content after the JSON value, or two JSON
  objects in one body → **400**, `unexpected content after request body`.
  The request is rejected in full; nothing is stored.
- **J17 (tip consistency).** Contradictory tip fields → **400**: empty mode
  with a non-zero amount or percent ("tip mode is none but amount_cents
  is set"); `amount` mode with a non-zero percent ("tip mode is amount
  but percent is also set"); `percent` mode with a non-zero amount ("tip
  mode is percent but amount_cents is also set"). Legitimate combinations
  (amount mode + amount only, percent mode + percent only, empty mode +
  nothing) still succeed (201).

**Known non-goals (document, don't test as failures):**

- No authentication or authorization on any endpoint — the exercise's
  restaurant simulation has no user model, so "anyone can advance any
  order" is in scope for the take-home, not a finding. In production this
  would be the first thing to add; the plan notes it rather than grading it.
- No rate limiting. A flood of creates would grow the in-memory store —
  acceptable for the exercise, noted for a real deployment.

## K. Concurrency

- **K1 (cancel-vs-advance race, corrected).** The previous version of this
  case was wrong: it fired many advances and expected exactly one winner,
  but sequential advances legitimately chain (Started → Ready for Pickup
  → Delivered — see K1b). The correct race is ONE cancel against ONE
  advance on the same Not Started order: exactly one returns 200 and the
  other returns 409, and the final status matches the winner (Started or
  Cancelled), never both, never corrupted. Run 10 rounds with a fresh
  order per round. (The unit suite also covers this deterministically;
  this is the live-service confirmation.)
- **K1b (advances chain).** Four sequential advances on one order: 200,
  200, 200, then 409 at Delivered; final status Delivered. This is the
  behavior that makes the old K1 wording invalid.
- **K2.** Fire 20 simultaneous `POST /orders` with the *same* idempotency
  key and identical bodies → exactly one order is created; all responses
  return the same order id.

---

## Results — executed Thu Nov 12, 2026 (afternoon PT) against `b32c7d4`

| Case | Expected | Actual | Pass? |
| --- | --- | --- | --- |
| A1 | menu lists 3 sizes, 12 toppings in category, 2 promos | menu shows Small $5.00 · Medium $7.00 · Large $12.00; basic 8% ($0.40/$0.56/$0.96) all 6; premium 11% ($0.55/$0.77/$1.32) all 6; ILOVEPIZZA + SAVE5 with descriptions | Pass |
| A2 | subtotal $24.00, tax $1.56, total $25.56 | subtotal $24.00, tax $1.56, total $25.56 | Pass |
| A3 | subtotal $17.76 | subtotal $17.76 (total $18.91) | Pass |
| A4 | subtotal $8.30 | subtotal $8.30 (total $8.84) | Pass |
| A5 | subtotal $8.33, tax $0.54, total $8.87 | subtotal $8.33, tax $0.54, total $8.87 | Pass |
| A6 | subtotal $39.96 | subtotal $39.96 (total $42.56) | Pass |
| A7 | case/underscore variants price identically | `add LARGE 1 extra_cheese` → 1 × Large (Extra Cheese), $12.96 subtotal | Pass |
| B1 | subtotal $14.64, tax $0.95, total $15.59 | subtotal $14.64, tax $0.95, total $15.59 | Pass |
| B2 | discount −$2.46, tax $1.44, tip $4.72, total $28.34 | discount −$2.46, tax $1.44, tip $4.72, total $28.34 | Pass |
| B3 | tip on post-discount post-tax total ($4.72, not $4.93) | tip $4.72 on $23.62 bill (tip was $5.25 pre-promo, recomputed to $4.72 after promo) | Pass |
| B4 | subtotal $20.12, discount −$5.00, tax $0.98, total $19.10 | API: subtotal 2012, discount 500, tax 98, tip 300, total 1910 ($19.10); builder submit: same $19.10 | Pass |
| C1 | total $4.79 | discount −$0.50, tax $0.29, total $4.79 | Pass |
| C2 | SAVE5 rejected at exactly $20.00 | builder: "promo does not qualify for this order: SAVE5 requires a pre-tax total over $20.00 (this order is $20.00)"; API: 400, same message | Pass |
| C3 | SAVE5 applies at $20.12, discount $5.00 | discount −$5.00, tax $0.98 (builder total $16.10 without tip; API with $3 tip $19.10) | Pass |
| C4 | second promo refused in both orders | "promo SAVE5 cannot be combined with ILOVEPIZZA; clear the current promo first" and reverse | Pass |
| C5 | unknown code rejected | `unknown promo code "PIZZA20"`, totals unchanged ($5.33) | Pass |
| C6 | promo clear restores no-promo totals | after clear: subtotal $5.00, tax $0.33, total $5.33 | Pass |
| D1 | +$3.50 | tip $3.50, total $14.15 on 2 Smalls | Pass |
| D2 | 20% on 2-Small bill ($10.65) → tip $2.13, total $12.78 | tip $2.13, total $12.78 | Pass |
| D3 | 0 and 100 accepted; 101 and −5 rejected | 0 → total $5.33; 100 → tip $5.33, total $10.66; 101/−5 → "Tip percentage must be a whole number between 0 and 100" | Pass |
| D4 | tip clear restores pre-tip total | after clear: total $5.33 | Pass |
| D5 | 3.005 rejected; 3.50 and $3.50 parse | "3.005" has more than two decimal places; `$3.50` → tip $3.50 | Pass |
| E1 | remove updates total | removed Small; remaining Medium plain, subtotal $7.00, total $7.46 | Pass |
| E2 | setqty recomputes | 3 × Small, subtotal $15.00, total $15.98 | Pass |
| E3 | settoppings none/replace recomputes | `none` → Large plain; `pineapple` → Large (Pineapple) | Pass |
| E4 | invalid size rejected | `Unknown size "huge". Sizes: Small, Medium, Large.` order unchanged | Pass |
| E5 | unknown topping rejected | `unknown topping "anchovies"` — order unchanged | Pass |
| E6 | invalid qty rejected | 0/−1/abc → `Quantity must be a whole number of at least 1` | Pass |
| E7 | unknown command hint | `Unknown command "frobnicate" — type 'help' for the list.` session continues | Pass |
| E8 | describe on empty order | `The order is empty.` + `Total: $0.00 (order is empty)` | Pass |
| E9 | edits blocked while pending | `That change was not applied: the previous submission's outcome is still unknown.` (add, remove, setqty all blocked) | Pass |
| E10 | signed fractions rejected | `1.-1` / `1.+1` → "has invalid digits in the fractional part" | Pass |
| E11 | arity usage errors, no requests | status/update/cancel/describe/submit with wrong args → `Usage: ...` each; `submit extra` → "Nothing was sent." (no order created) | Pass |
| E12 | huge dollar amount rejected | `184467440737095517` → "exceeds the maximum amount of $1000000000.00" | Pass |
| F1 | 201, Not Started, server totals; client totals rejected | 201, ord-0001, totals subtotal 2012 / discount 500 / tax 98 / tip 300 / total 1910; client `totals` field → 400 (see J1) | Pass |
| F2 | GET → 200 with order + status | 200, ord-0001, Not Started, totals as created | Pass |
| F3 | PATCH while Not Started → 200 | 200, replaced with Large+Bacon, totals recomputed ($14.19) | Pass |
| F4 | advance → 200, Started | 200, status Started | Pass |
| F5 | PATCH/cancel after Started → 409 | both 409 with "can only be changed/cancelled while Not Started" | Pass |
| F6 | advance → Ready for Pickup, then Delivered | 200 Ready for Pickup, 200 Delivered | Pass |
| F7 | advance/cancel/update past Delivered → 409 | all three 409 ("cannot be advanced", "can only be cancelled/changed while Not Started") | Pass |
| F8 | cancel while Not Started → 200; then all ops 409 | cancel 200, status Cancelled; advance/update/cancel → 409 each | Pass |
| F9 | unknown id → 404 | GET/PATCH/cancel/advance on ord-9999 → 404 each | Pass |
| F10 | auto-advance moves orders per interval | `-auto-advance 2s`: order reached Delivered within 5s | Pass |
| G1 | first 201, replay 200 same order | 201 ord-0003, replay 200 ord-0003, identical body | Pass |
| G2 | same key, different body → 409 naming original order | 409: `idempotency key already used for a different order: key "g1" created order ord-0003; retry that exact request or use PATCH /orders/ord-0003 to change it` | Pass |
| G3 | no key → new order each time | ord-0004, ord-0005 (distinct) | Pass |
| G4 | fault-injected lost reply → retry recovers same order, exactly one created | proxy dropped first 2 POST /orders replies; builder reported EOF/unresolved, kept key, paused editing; 3rd submit → "The service already had this order (idempotent replay) — no duplicate was created." ord-0040, $8.28; server-side only ord-0040 exists (0041–0043 absent). Unit test `TestRetryRecoversExactlyOneOrder` (6 failure modes) also PASS | Pass |
| G5 | truncated/empty/contentless 2xx reported as error | Unit tests `TestCallReportsBodyProblems`, `TestSubmitRejectsContentlessSuccessBody`, `TestStatusAndCancelRejectContentlessBodies` all PASS; manual proxy variant not run in this pass | Pass (unit-test verified) |
| G6 | replay after PATCH: original body 200, patched body 409 | re-POST original (Small ×1) with key → 200, order returned in current (patched) state; re-POST patched contents (Large ×2) → 409 naming ord-0006 | Pass |
| H1 | qty > 1,000,000 rejected | 400: "quantity 1000001 exceeds maximum of 1000000 per line item" | Pass |
| H2 | monetary maximums enforced | 100 lines × 1M Large → 400 subtotal $1,200,000,000.00 exceeds max; 83 lines → 400 order total $1,060,740,000.00 exceeds max; 2 lines × 1M Large → 201, total $25,560,000.00 | Pass |
| I1 | malformed JSON → 400 | 400: "invalid request body: invalid character 'b' looking for beginning of object key string" | Pass |
| I2 | empty body → 400 | 400: "invalid request body: EOF" | Pass |
| I3 | invalid order → 400 | unknown size / unknown topping / zero qty → 400 with specific messages | Pass |
| J1 | client totals/status/id → 400 unknown field; nothing created | `totals`/`status`/`id` → 400 `json: unknown field "..."`; nested `amount_cent` and `crust` → 400; next order id absent after rejected POST | Pass |
| J2 | PATCH with totals/status → 400; store unchanged | 400 unknown field; re-GET before/after byte-identical | Pass |
| J3 | dup topping → 400 | 400: "topping Bacon is listed twice on the same pizza" | Pass |
| J4 | empty/missing items → 400 | both → 400: "order must contain at least one line item" | Pass |
| J5 | non-integer numbers → 400 | 1.5, 1e2, "1", 1.00000000000000001, amount_cents 100.000000000000001 → 400 each | Pass |
| J6 | negative tip / pct out of range → 400 | −500 → 400 "tip amount cannot be negative"; pct 101 → 400 | Pass |
| J7 | unknown tip mode → 400 | 400: "unknown tip mode" | Pass |
| J8 | promo case/whitespace canonicalized; unknown → 400 | `ilovepizza` / ` ILOVEPIZZA ` → stored ILOVEPIZZA, total 479; `save99` → 400 | Pass |
| J9 | qty 0/negative/1000001 → 400 | 0 → 400 (see I3); 1000001 → 400 (see H1) | Pass |
| J10 | wrong method → 405 | DELETE /orders → 405; GET /orders/{id}/cancel → 405 | Pass |
| J11 | unknown route / trailing slash → 404 | /nope → 404; POST /orders/ → 404 | Pass |
| J12 | oversized body → 400 or 413 (see case text) | 2 MB single JSON value → 400 "invalid request body: http: request body too large"; valid JSON + 2 MB trailing → 413 "request body too large" | Pass |
| J13 | traversal-style ids → 404 | `..%2F..%2Fetc` → 404; `/orders/{id}/extra` → 404 | Pass |
| J14 | no Content-Type → still processed (leniency) | 201 with no Content-Type header | Pass |
| J15 | healthz → 200 ok | 200 `{"status": "ok"}` | Pass |
| J16 | trailing content / two objects → 400 | 400: "unexpected content after request body" | Pass |
| J17 | contradictory tip fields → 400; legit combos 201 | amount+percent → 400; percent+amount → 400; none+amount → 400; legit amount-only / percent-only → 201 | Pass |
| K1 | 1 cancel vs 1 advance, 10 rounds: exactly one 200 + one 409 each | 10/10 rounds exactly one winner (6 advance-wins, 4 cancel-wins); final status matched the winner every round | Pass |
| K1b | sequential advances chain 200,200,200,409 | 200 → Started, 200 → Ready for Pickup, 200 → Delivered, 409 past Delivered | Pass |
| K2 | 20 simultaneous same-key creates → one order | all 20 responses returned ord-0038; exactly one distinct id | Pass |

## Scope notes (what this plan deliberately does not cover)

- Load/performance testing (single in-memory service, out of scope).
- Behavior across service restarts (storage is documented as in-memory).
- The interactive loop's keystroke-level UX (covered at the command level
  in section E, not per-keystroke).
