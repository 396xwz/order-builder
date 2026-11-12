# Pizza-Driven Development

An order builder and order service for a pizza restaurant, written in Go
using only the standard library. A customer assembles an order in a
command-line builder, watches the total update as they change it, and
submits it to a separate order service over HTTP; they can then check,
change, or cancel the order until the kitchen starts it.

## Running it

Prerequisite: Go 1.23 or newer. There are no third-party dependencies.

**Order service** (start this first):

```sh
go run ./cmd/orderservice            # listens on :8080
```

Optional: simulate the kitchen advancing every open order one step per
interval (off by default):

```sh
go run ./cmd/orderservice -auto-advance 30s
```

**Order builder** (in a second terminal):

```sh
go run ./cmd/builder                 # type 'help' at the prompt
```

**Tests:**

```sh
go test ./...
```

Building binaries: `go build -o orderservice ./cmd/orderservice` and
`go build -o builder ./cmd/builder` produce the two executables.
(`go build ./...` compiles everything but does not write named binaries.)

### A taste of the builder

```
pizza> add large 1 bacon,pineapple
Added line 1.
  Subtotal: $14.64
  Tax (6.5%): $0.95
  Total: $15.59
pizza> tip percent 20
pizza> promo ILOVEPIZZA
Promo ILOVEPIZZA applied.
  Subtotal: $24.64        (after also adding 2 smalls)
  Discount (ILOVEPIZZA): -$2.46
  Tax (6.5%): $1.44
  Tip: $4.72
  Total: $28.34
pizza> submit
Order ord-0001 submitted. Status: Not Started. Total charged: $28.34.
```

The restaurant side advances an order with
`POST /orders/{id}/advance` (or the `-auto-advance` timer above).

## Order service API

All request and response bodies are JSON. Monetary values are integer
cents in fields named `*_cents`, plus a `total_formatted` convenience
string. Errors are `{"error": "..."}` with a human-readable message.

| Method & path | Purpose | Success | Errors |
| --- | --- | --- | --- |
| `POST /orders` | Submit an order; send an `Idempotency-Key` header to make retries safe | 201 (200 if the key was already used — the original order is returned) | 400 invalid order |
| `GET /orders/{id}` | Fetch an order and its status | 200 | 404 |
| `PATCH /orders/{id}` | Replace the order's items/tip/promo wholesale (Not Started only) | 200 | 400, 404, 409 |
| `POST /orders/{id}/cancel` | Cancel the order (Not Started only) | 200 | 404, 409 |
| `POST /orders/{id}/advance` | Restaurant side: advance one lifecycle step | 200 | 404, 409 |

Submit an order:

```http
POST /orders
Idempotency-Key: demo-1
Content-Type: application/json

{"items": [{"size": "Medium", "quantity": 2, "toppings": ["Extra Cheese"]},
           {"size": "Small", "quantity": 1}],
 "tip": {"mode": "amount", "amount_cents": 300},
 "promo_code": "SAVE5"}
```

Response (201):

```json
{"id": "ord-0001",
 "items": [{"size": "Medium", "quantity": 2, "toppings": ["Extra Cheese"]},
           {"size": "Small", "quantity": 1, "toppings": null}],
 "tip": {"mode": "amount", "amount_cents": 300},
 "promo_code": "SAVE5",
 "totals": {"subtotal_cents": 2012, "discount_cents": 500,
            "taxable_cents": 1512, "tax_cents": 98, "tip_cents": 300,
            "total_cents": 1910},
 "status": "Not Started", "total_formatted": "$19.10", "...": "..."}
```

Trying to cancel after the kitchen has started the order (409):

```json
{"error": "operation not allowed in the order's current state: order ord-0001 is \"Started\" and can only be cancelled while \"Not Started\""}
```

The request body for `POST /orders` and `PATCH /orders/{id}` is the
order's contents only — items, tip, promo code. There is deliberately no
totals field: the service computes totals itself.

Both endpoints enforce the same strict request contract, and a refused
request never changes stored state:

- **Exactly one JSON object.** Trailing content or a second object is
  rejected; the body-size limit (1 MiB) is enforced while consuming the
  request, not inferred from one successful parse.
- **No unknown fields at any level.** `{"totals":…}` is rejected rather
  than ignored, and so is a misspelled nested property such as
  `{"tip":{"amount_cent":300}}`, which would otherwise be read silently
  as "no tip".
- **Integer tokens stay integers.** The body is decoded directly into the
  typed request struct, so `{"quantity":1.00000000000000001}` and
  `{"amount_cents":100.000000000000001}` are rejected instead of being
  rounded into valid-looking values. Money and quantities are integers
  end to end, including at the decoding boundary.
- **The `Tip` object must be internally consistent.** When mode is `""`
  (no tip), `amount_cents` and `percent` must be zero; when mode is
  `"amount"`, `percent` must be zero; when mode is `"percent"`,
  `amount_cents` must be zero.

`POST /orders` accepts an optional `Idempotency-Key` header. The service
records what each key originally created and compares later requests
against **that original creation request**, never against the order's
current contents — so an intervening `PATCH` cannot change what a key
means. Replaying the original request returns the order's current state
with its original ID and HTTP 200. Sending *different* contents under a
used key is HTTP 409, and the error names the original order ID so the
client is never stranded without it. An absent key opts out of
idempotency entirely: every such call creates a new order.

## Assumptions

Every place the requirements left a decision open, the decision made, and
where it lives in the code:

1. **Order of operations.** Subtotal → promo discount (on the pre-tax
   subtotal) → tax (6.5% of the post-discount subtotal) → tip → total.
   The spec fixes the discount as "before tax" and tax as "of the total
   order"; taxing the discounted amount is the natural reading.
2. **Percentage tip base.** "% of the total bill" is read as the
   post-discount, post-tax total — the amount actually billed before tip.
   A dollar tip is taken as given. Percentage tips are limited to 0–100%.
3. **Rounding.** All money is integer cents; the only rounding is
   round-half-up, applied in exactly three places (percentage discount,
   tax, percentage tip) at the order level, never per line. Topping
   prices need no rounding on this menu — 8% and 11% of $5/$7/$12 land on
   exact cents for every size — and a test asserts that, so a future size
   that breaks it is caught rather than silently rounded.
4. **"Over $20" is strict.** SAVE5 requires a pre-tax subtotal *greater
   than* $20.00; exactly $20.00 does not qualify. Tested at the boundary.
5. **An unqualified promo is rejected, not ignored.** Applying SAVE5 to a
   $20-or-less order (at submit, update, or at set time when the order
   already has items) is an error with a clear message. Exception: setting
   a promo on an empty order always succeeds — it cannot be qualified
   until items are added. Adding items later that should un-qualify the
   promo is caught at the next submit or update. The customer is never
   silently denied a discount they think they have — nor silently charged
   without one.
6. **Promo conflicts are errors.** SAVE5 and ILOVEPIZZA cannot combine;
   applying one while the other is set fails with a message telling the
   customer to clear the current promo first. Nothing stacks, nothing is
   silently replaced.
7. **Updates are wholesale replacement.** `PATCH` replaces items, tip,
   and promo entirely; there are no partial updates. An update that would
   drop a SAVE5 order to $20 or less is rejected per assumption 5.
8. **One of each topping per pizza.** Listing the same topping twice on
   one line item is an input error, not a double charge (the menu has no
   "double pepperoni" concept). Toppings apply per pizza, so a topping's
   price is multiplied by the line's quantity along with the base price.
9. **Cancelled is a terminal status.** The spec's four statuses are the
   kitchen lifecycle; a cancelled order additionally reports status
   `"Cancelled"`, keeps its record, and rejects all further changes.
10. **Status advancement is restaurant-triggered.** Via the `advance`
    endpoint per order, or the opt-in `-auto-advance` timer (default
    off). Orders never advance on their own otherwise.
11. **Idempotency is opt-in by header.** `POST /orders` without an
    `Idempotency-Key` creates a new order every time (documented here and
    in the API table). With a key, retries return the original order.
    Keys are remembered for the life of the process.
12. **Storage is in-memory.** Orders live for the life of the service
    process; restarting the service forgets them. Order IDs are
    sequential (`ord-0001`, …), which is convenient for a demo and would
    be replaced by unguessable IDs in production.
13. **No authentication**, per the constraints. Anyone who can reach the
    service can advance orders; the advance endpoint is a simulation of
    the restaurant side, not a secured staff API.
14. **Orders have declared numeric limits.** A line item's quantity must
    be in `[1, 1,000,000]` (`pricing.MaxQuantity`), and every monetary
    figure — each line total, the subtotal, and the final total — must be
    in `[$0.00, $1,000,000,000.00]` (`pricing.MaxCents`). These are a
    domain decision, not an artifact of `int64`: the bounds sit far below
    the point where the arithmetic could overflow, and the `safe*` helpers
    additionally check `int64` overflow as a backstop so a future menu or
    limit change cannot silently wrap. Input outside the range is
    **rejected with an error**, never clamped or wrapped — including a
    total that only exceeds the limit once tax and a dollar tip are added
    to an otherwise-legal subtotal. The CLI applies the same bounds when
    parsing a typed dollar amount, before the conversion to cents.

## Design write-up

### Money and the single source of truth for pricing

Money is integer cents (`pricing.Cents`) everywhere — never a float —
formatted to dollars only for display. One package, `internal/pricing`,
holds the menu and promos as data and a single `Price` (assumption 1):

```
subtotal = Σ quantity × (base_price + Σ topping_rate × base_price)
discount = promo applied to the pre-tax subtotal, if it qualifies
taxable  = subtotal − discount
tax      = round_half_up(6.5% of taxable)
tip      = dollar amount as given, or round_half_up(percent of taxable + tax)
total    = taxable + tax + tip
```

One rounding rule — round half up, in three places (percentage discounts,
tax, percentage tips) — via `den/2` before integer division. That is only
round-half-up for a **non-negative** amount, so the helper rejects
negatives rather than applying a second, undocumented rule. Quantities
are bounded to `[1, 1,000,000]` and money to `[$0, $1,000,000,000]`,
rejected not clamped (assumption 14).

Both processes price from the **same** package, so the builder totals
locally with no round-trip. Requirement 2 holds narrowly: identical
contents cannot price differently (one `Price`, one menu), and the
service recomputes the charge from stored contents, trusting no client
total. Strict decoding does *not* prove it — a well-formed request for
the wrong pizza is still well-formed; what ties charge to draft is that
the builder freezes the submitted contents and resends that snapshot on
retry, refusing edits until resolved. While unresolved, no charged total
is claimed.

### Extensibility: menu and promos as data

A **size** is one row (name, base price); a **topping** one row (name,
category); a **topping category** one row (name, percentage of base),
whose toppings then price automatically, since a topping's price derives
from its category rate and the base price. No pricing code names a
specific size, topping, or category.

A **promo** is a record: kind (percent-off or fixed-off the pre-tax
subtotal), parameters, an optional minimum pre-tax subtotal (strictly
greater — SAVE5's "over $20"), and conflicting codes. The engine reads
records generically: discount if qualified, floor a fixed discount at the
subtotal, refuse conflicts from the records' own lists (symmetry tested).
So "$8 off over $50, never with SAVE5" is a record, not a branch:

```go
{
    Code:               "BIGORDER8",
    Kind:               PromoFixedOff,
    Amount:             800,
    HasMin:             true,
    MinPreTaxExclusive: 5000,
    Conflicts:          []string{"SAVE5"},
    Description:        "$8 off when the pre-tax total is over $50",
},
```

One row in `promoList`, no pricing algorithm changes required;
configuration entries live in Go source, and the symmetry test requires
the matching entry on `SAVE5`.

"Data-driven" is easy to overclaim, so: new *mechanisms* ("cheapest pizza
free", BOGO) need a new `PromoKind` and a discount-function case; new
*condition types* (item count, size, date range) need a new field and
evaluation logic, minimum pre-tax subtotal being the only one; and
exclusion is pairwise and total — at most one promo per order, so
"stacks only with that one" is a different model and application order is
unmodelled. Conditions and conflicts are pure data; mechanisms are not.

### Order lifecycle, retries, and races

An order moves `Not Started → Started → Ready for Pickup → Delivered`,
advanced only by the restaurant side; a customer can update or cancel it
only in `Not Started`. Every mutation checks status and mutates while
holding the store's single mutex, so each check-and-change is atomic.

*Retry after a timeout.* The builder sends one idempotency key per order.
On a timeout — the order may or may not have arrived — it retries once
with the **same** key, saying so. The service records key → order ID on
first submission, so the retry returns the original order (200, not 201)
rather than a duplicate: at most one order per key while the service
runs.

*Ambiguous outcomes.* Whenever it cannot prove the order was **not**
created, the builder keeps the key and submitted contents, refuses local
edits (so one key never means two orders), and `submit` resends that
snapshot. The key is released only for a response that *establishes*
non-creation — narrower than "any 4xx":

| Response | Treated as | Why |
| --- | --- | --- |
| 200, 201 | Resolved | Created, or an idempotent replay. |
| Validation rejection (400) | Non-creation | Judged before storing. |
| 409 | Key in use | Proof it *was* used; original ID reported. |
| 408, 425 | Ambiguous | A proxy can emit these post-storage. |
| 5xx | Ambiguous | May follow creation upstream. |
| Other 2xx (202, 204) | Ambiguous | 202 is "queued, not stored". |
| Timeout / unreadable / contentless | Ambiguous | Says nothing reliable. |

A 200 or 201 must also carry `id`, `status`, and `total_formatted` before
success is announced. Limits: a validation rejection is **inferred, not
authenticated**, since a proxy's own 403 is indistinguishable on the wire
from the service judging the contents; the key is **in memory**, so
restarting mid-outcome loses it and a later submission could duplicate;
and `status` **resolves nothing**, reporting ID, status and total but
never contents, so it cannot confirm *which* contents an uncertain
`PATCH` stored — the builder says so and prints the `GET /orders/{id}`
that does show them.

*Cancel as the kitchen starts.* Both serialize on the mutex and validate
status inside the lock. One wins; the other sees the new status —
`Started` blocks the cancel, `Cancelled` blocks the advance — and gets a
409 naming it. Guarantee: exactly one winner, never an order both
cancelled and started, raced 100 times under `-race`.

### What was cut, and what comes next

Cut to stay in the timebox, each a documented assumption rather than a
hidden gap: persistence; kitchen/delivery integration (advancement is an
endpoint); authentication; partial updates; order versioning (updates are
last-writer-wins); a menu admin UI (the menu is a data table). Next, in
order:

1. **Persist orders and idempotency keys** — the one cut with a
   correctness consequence, not just a convenience cost: a restart
   forgets the key→order record, so a retry afterwards can duplicate. It
   maps onto a SQL table plus a uniqueness constraint, but is a
   *substitution*, not a drop-in: `Store` is concrete, and every method
   would start returning errors.
2. **Unguessable IDs and auth on restaurant-side endpoints**, as IDs are
   sequential (`ord-0001`) and `/advance` is unauthenticated.
3. **Order versioning (ETag / `If-Match`)** so a stale update gets a 409
   instead of silently overwriting.
4. **Structured logging and metrics**, which would reveal whether the
   ambiguous paths above ever fire in production.

## Testing

Run with `go test ./...` or `go test -race ./...`.

Packages tested and what they cover:

### `internal/pricing` — arithmetic, menu, and validation

- Every size × topping-category price computed by hand, including
  rounding (6.5% of $14.28 = 92.82¢ → 93¢, 10% of $20.12 = 201.2¢ →
  201¢). Round-half-up is tested edge-to-edge.
- Three full orders computed by hand from the spec.
- The SAVE5 boundary: exactly $20.00 does not qualify; $20.01 does.
- Promo mutual exclusion, including symmetry of conflict data.
- Invalid input: unknown size/topping/promo, zero quantity, duplicate
  topping, out-of-range tip, negative tip amount.
- Overflow: `safeAdd`, `safeMul`, `safePercentOf` reject arithmetic
  overflow; `LineTotal` and `Price` bound quantities and subtotals.
- Helper contracts, including the cases they are easy to get wrong:
  `safeMul` rejects a negative multiplier even when the other operand is
  zero (no short-circuit hiding bad input) and bounds a negative operand
  against the negative limit `MinInt64/a` rather than by magnitude, so
  the representable boundary products `MinInt64 × 1` and
  `(MinInt64/2) × 2` are accepted while `MinInt64 × 2`,
  `(MinInt64/2) × 3`, and `(MinInt64/2 - 1) × 2` are rejected; `a == -1`
  is special-cased because `MinInt64 / -1` is the one int64 division that
  overflows on its own. `safePercentOf` rejects a non-positive denominator, a
  negative rate, and a negative amount, and its overflow bound accounts
  for the rounding term `percentOf` adds before dividing.
- Declared limits at their exact boundaries: quantity at `MaxQuantity`
  and `MaxQuantity+1`, zero and negative quantities, a tip at and above
  `MaxCents`, and a **total** pushed past `MaxCents` by tax and tip even
  though the subtotal is legal. Every figure in a computed `Totals` is
  asserted to lie within `[0, MaxCents]`.
- `Cents.String` handles the `MinInt64` negation edge case.

### `internal/order` — model and lifecycle

- Every `Status.Next` outcome and terminal-state detection.
- Deep-copy isolation.

### `internal/store` — lifecycle enforcement, idempotency, concurrency

- Update/cancel rejection for every non-`Not Started` status (Started,
  Ready for Pickup, Delivered, Cancelled).
- Atomic validation failure: for nine distinct invalid updates (unknown
  size, zero and over-maximum quantity, unknown and duplicate topping, no
  items, unknown and unqualified promo, out-of-range tip) the stored
  order is compared in full with `reflect.DeepEqual` — not a field or two
  — and `UpdatedAt` is asserted not to move.
- Copy isolation in both directions, for **every** accessor: mutating the
  result of `Create`, `Get`, `Update`, `Cancel`, or `Advance` cannot reach
  the store; two `Get` results do not share a toppings array; and
  mutating the caller's slices after `Create`/`Update` does not change
  what was stored.
- Idempotent create (same key → original order, no duplicate), including
  concurrent idempotency: 10 goroutines with one key produce one order,
  **every** caller receives that same ID with no error, and **exactly
  one** is told `created == true`.
- Concurrent mixed operations (cancel, advance, update, get on one order,
  50 rounds) leave a consistent final status, with any failure being an
  explicit `ErrStateConflict` rather than a panic or a lost order.
- Same-key/different-body rejection (`ErrIdempotencyConflict`) with the
  original order ID in the error message.
- Replay is compared against the original creation request: after a
  `PATCH`, replaying the original request still returns the same order,
  while the post-update contents under that key are refused.
- Replay still works once the order is Cancelled or Delivered, so a late
  retry cannot create a second order.
- The recorded creation request is a private copy: mutating the caller's
  slices afterwards does not change what the key means.
- Create without key always creates a new order.
- `AdvanceAll` advances each eligible order exactly one step, skips
  terminal states, and returns the right count — for an empty store, a
  uniform store, and a **mixed-state** store holding one order in each of
  Not Started, Started, Ready for Pickup, Delivered, and Cancelled, where
  each order's resulting status is asserted individually.
- The cancel-vs-advance race yields exactly one winner (100 rounds).

### `internal/server` — HTTP handler invariants

- Submitted totals agree with the shared pricing package for the same
  input, cent for cent.
- Idempotent replay over HTTP returns 200 instead of 201.
- Lifecycle conflicts return 409 with a readable message.
- Bad promos and unqualified promos return 400.
- Trailing content and a second JSON object are rejected with 400.
- Unknown fields are rejected with 400 at the top level *and* nested
  (`{"tip":{"amount_cent":300}}`, unknown item fields).
- Non-integer numeric tokens are rejected rather than rounded —
  `quantity:1.00000000000000001`, `amount_cents:100.000000000000001`,
  `1.5`, `1e2`, and a quoted `"1"`.
- Tip mode/field contradictions are rejected with 400, across every
  mode/field pairing: none-with-amount, none-with-percent, none-with-both,
  amount-with-percent, percent-with-amount, an unknown mode,
  out-of-range values in the active field, and *negative* values in an
  inactive one (so the check is "exactly zero", not merely "not
  positive"). The accepted cases — omitted tip, explicit none, zero
  amount, zero percent, percent at exactly 100, and each mode with its
  inactive field written out as an explicit `0` — are asserted too, so
  the validation is not merely strict.
- `GET` reports every lifecycle status (Not Started, Started, Ready for
  Pickup, Delivered, Cancelled), each with a formatted total, and
  advancing a Delivered order returns 409.
- `PATCH` enforces the identical contract, and a refused `PATCH` leaves
  the order byte-for-byte unchanged (verified by re-reading it).
- Body-size limits: a large but legal body is accepted; an oversized one
  is refused and leaves the store unchanged.
- Rejected requests leave the store unchanged.
- Same-key/different-body returns 409 naming the original order, while
  replaying the original request (including after a `PATCH`) returns 200
  with that order.

### `cmd/builder` — parsing, session logic, and the HTTP client

Pure parsing and formatting (`main_test.go`):

- `parseMoney` accepts valid dollar amounts and rejects invalid forms
  (bad signs, too many decimals, garbage) **and** amounts that would
  overflow the conversion — including the reproduced
  `184467440737095517` case, which previously displayed `$0.84`. The
  supported maximum and just-over-maximum are both covered.
- `parseToppings` normalizes names, handles separators and whitespace.
- `Describe` formats orders correctly (toppings, plain, totals).
- `showTotals` shows subtotal, discount (if any), tax, tip, and total.
- Local editing commands (add, remove, setqty, settoppings, tip, promo)
  update state and show the new totals; dispatch routes commands and
  enforces command arity.
- Editing is blocked while a submission's outcome is unresolved.
- Invalid arity on a network command (`submit unexpected`,
  `cancel nonexistent-id unexpected`, `status a b`, `update x`) reports
  usage and issues **no request** — asserted against the server — so
  excess arguments can never cancel the session's own order.
- `cancel` and `status` each use the session's order by default and an
  explicit ID when given — both asserted on the path the server actually
  received — an explicit ID does not become the session's order, and with
  no ID and nothing submitted they make no request.

Real HTTP client and submit state machine (`client_test.go`, driven by
in-process `httptest` servers — no live service and no real timers
beyond short client timeouts):

- `apiClient.call` reports empty, malformed, and truncated bodies, and
  non-JSON error pages, as errors rather than success, keeping the HTTP
  status in the message.
- A body that stops arriving is classified as a timeout, so submit's
  retry branch fires.
- A contentless 2xx (`{}` or `null`) is never announced as success and
  never overwrites the session's order ID.
- The idempotency key and submitted snapshot survive every ambiguous
  outcome (timeout, unreadable 201, 5xx), and the retry reuses both —
  asserted by inspecting the keys and bodies the server actually
  received, so a duplicate-creating regression fails the test.
- A 4xx read as a service validation rejection (400 on a bad promo)
  releases the key and re-enables editing; a 409 keeps the key and
  surfaces the original order ID; 408 and 425 keep it, because they do
  not establish non-creation. `ambiguousRequestFailure` is tested
  directly on both the ambiguous and the judging status codes.
- Any 2xx other than 200/201 (202, 204, 205, 206) is treated as
  unresolved rather than announced as a completed order, even when the
  body looks like a valid order.
- A timeout triggers exactly one automatic retry with the same key, and
  leaves the submission pending if that also fails.
- `update` advises that retrying is safe after an ambiguous failure (a
  PATCH is a wholesale replacement) and that it will *not* help after a
  409; repeating an update is shown to send an identical body. After an
  unreadable or contentless reply, the advice is asserted *not* to claim
  that `status` shows the stored contents, and to name the
  `GET /orders/{id}` call that does.
- `submit` on an already-submitted order makes no request and directs
  the customer to `update`; an empty order makes no request at all.
- `status` and `cancel` refuse to confirm anything from a contentless
  reply, and a 502 with an HTML body is not reported as "unreachable".

### End-to-end recovery (`cmd/builder/recovery_test.go`)

These drive the **real builder against the real service** through a
fault-injecting proxy; everything below the proxy is production code. The
proxy always delivers the request and then damages only the reply, which
is what makes the duplicate-order scenario reachable.

- For a lost body, truncated JSON, a 502, a 408 issued *after* the order
  was stored, a contentless 200, and a 202, the retry leaves **exactly one
  order** in the service, and the ID the customer ends up holding is that
  order. This is the concrete anti-duplication guarantee.
- The total the builder printed is compared against the total the service
  actually stored.
- While an outcome is unresolved, every edit is refused, and the contents
  the service stored are then compared field-by-field with what the
  builder still shows locally.
- A server-side rejection stores nothing, releases the key, and a later
  submission creates exactly one order.

Not covered: the CLI's interactive read loop itself (`main`'s scanner —
the commands it dispatches are tested directly), load/performance, and
behavior across service restarts (storage is documented as in-memory, and
the restart-duplication limit is stated in the retry section above rather
than tested). The fault injection models damaged *responses*; it does not
model partially-written requests or a service that crashes mid-write.

### Why these invariants

Each group of tests is aimed at a specific way a pizza order can go wrong
for a real customer, not at a coverage number:

- **Pricing tests prevent incorrect charges.** Money is the output the
  customer is held to, so the hand-computed orders, the round-half-up
  edges, the SAVE5 boundary, and the overflow guards exist to stop a
  wrong total — by a cent or by a wrap — from ever being quoted or
  stored.
- **Lifecycle and concurrency tests prevent illegal state changes.** The
  per-status rejection tests and the racing cancel/advance rounds keep an
  order from being edited after the kitchen has started it or from
  landing in a status the state machine does not permit, even when two
  requests arrive at once.
- **Retry tests prevent duplicate orders.** The idempotency-key, replay,
  and fault-injected recovery tests all protect one guarantee: a customer
  who retries an ambiguous submission is charged for one pizza order, not
  two.
- **Validation and copy-isolation tests protect stored data.** Strict
  decoding and atomic validation failure keep a malformed or partially
  valid request from mutating a stored order, and the deep-copy
  assertions keep a caller's later edits to its own slices from silently
  rewriting what the store holds.

The omitted areas were deferred because they cost the most time while
protecting the least within a 3-hour timebox. The interactive read loop is
a thin `bufio.Scanner` wrapper over command handlers that are already
tested directly, so covering it would mostly test the standard library.
Load and performance work needs a target to be meaningful, and the
exercise sets none; the store is a mutex-guarded map whose contention
behavior would say little about a persisted implementation. Restart
behavior is not tested because there is nothing durable to restart into —
storage is in-memory by documented assumption, and the resulting
retry-after-restart duplication is stated as a known limit in the retry
section and listed first under future work, which is the honest treatment
until persistence exists. Each omission is therefore a stated boundary
rather than an untested assumption.

## AI usage note

Claude (Anthropic) wrote the first working implementation — structure,
pricing/service/builder code, and tests — from the exercise spec, and
Claude and GitHub Copilot were both used in the later review rounds that
produced the fixes; the commit history shows those as separate rounds.

- **One example of AI output that was rejected and fixed.** The API
  client discarded JSON decode errors (assigning them to `_`), so an
  empty or truncated 201 was announced as success, printing
  `Order  submitted. Status: . Total charged: .`. Fixing it took two
  passes — `apiClient.call` now returns body-read and decode errors
  wrapped with `%w` (so a body-read timeout still triggers the retry
  branch), and because `{}` and `null` still decoded cleanly,
  `orderResponse.validateOrder` now requires `id`, `status`, and
  `total_formatted` before any success is printed.

- **One invariant checked by myself rather than by trusting the AI.** The
  invariant is requirement 5's: *an unreliable connection must never
  leave the restaurant holding two orders.* Reading the code was not
  enough to settle it — the code looked correct. So it is checked by
  construction instead, in `cmd/builder/recovery_test.go`: the real
  builder talks to the real service through a proxy that always delivers
  the request and then damages only the reply. For each of six failure
  modes (lost body, truncated JSON, 502, a 408 issued after the order was
  stored, a contentless 200, and a 202) the test retries and then asserts
  directly against the service's own store that **exactly one** order
  exists and that the ID the customer holds is that order.

  That construction is what makes the claim falsifiable: reverting any of
  the fixes makes `store.Count()` return 2 and the test fail by naming
  the duplicate. The same method found the other defects — the
  false-success decoding, the dollar-conversion wrap (`tip amount
  184467440737095517` displaying `$0.84`), the replay-comparison
  inversion after a `PATCH`, and the `float64` round-trip that turned
  `quantity:1.00000000000000001` into quantity 1 — each of which the test
  suite of the time accepted. Every one is now pinned by a regression
  test.

## Repository layout

```
internal/pricing/   menu data, promo records, and the one Price function
internal/order/     order model and lifecycle statuses
internal/store/     in-memory store: idempotency + locked state machine
internal/server/    HTTP handlers (thin) over the store
cmd/orderservice/   the order service binary
cmd/builder/        the interactive order builder CLI
```
