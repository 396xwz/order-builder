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

Money is an integer number of cents (`pricing.Cents`) everywhere —
never a float — and is formatted to dollars only when displayed. All
pricing lives in one package, `internal/pricing`: the menu (sizes,
topping categories, toppings) as data tables, promo codes as data records
(see below), and a single `Price` function that computes the breakdown in
the documented order of operations.

There is exactly one rounding rule — round half up, applied in exactly
three places (percentage discounts, tax, percentage tips) — implemented
as integer arithmetic by adding `den/2` before dividing. That trick is
only round-half-up for a **non-negative** amount; on a negative value it
rounds half toward zero instead. Since every amount it is applied to is
non-negative by construction, the helper rejects a negative input rather
than quietly returning a figure rounded by a second, undocumented rule.

Pricing runs in **both** processes, from the **same** package. The
builder computes the running total locally so the customer sees it update
on every edit with no network round-trips.

`Price` computes, in this order (assumption 1, restated here so the
design answer stands on its own):

```
subtotal = Σ quantity × (base_price + Σ topping_rate × base_price)
discount = promo applied to the pre-tax subtotal, if it qualifies
taxable  = subtotal − discount
tax      = round_half_up(6.5% of taxable)
tip      = dollar amount as given, or round_half_up(percent of taxable + tax)
total    = taxable + tax + tip
```

Every figure is bounded: a quantity in `[1, 1,000,000]` and every
monetary figure in `[$0.00, $1,000,000,000.00]` (assumption 14), rejected
rather than clamped or wrapped.

Requirement 2 — "the total the customer sees must match the total the
restaurant charges" — holds as follows, stated precisely because a
blanket claim would be too strong:

- **Guaranteed by construction:** for identical contents, the two
  processes cannot compute different totals. There is one `Price`
  function and one menu table, so calculation drift is impossible by
  design, not by keeping two implementations in sync.
- **Enforced by the service:** the charged total is recomputed
  server-side on every submission and update, from the stored contents.
  No client-supplied total is ever trusted, because the request format
  has nowhere to put one.
- **Not proved by strict decoding.** Rejecting unknown fields, trailing
  content, and non-integer money tokens stops a *malformed or silently
  reinterpreted* request from being priced — `{"quantity":1.0000000001}`
  is refused rather than rounded to 1 — but a valid shape is not evidence
  that the body describes the draft the customer was looking at. A
  well-formed request for the wrong pizza is still well-formed. What
  actually ties the charge to the draft is narrower and mechanical: the
  builder freezes the submitted contents in a snapshot and resends that
  exact snapshot on every retry, refusing local edits until the outcome
  is known, so one idempotency key can never come to mean two different
  orders. Agreement on the *figure* then follows from the shared `Price`
  function, not from validation.
- **Depends on the reply being read correctly**, which response
  validation enforces: a submission is only reported as complete when
  the service returned 200/201 with a usable order body.
- **Not claimed:** that the figure on screen after an *unresolved*
  submission is the charged total. In that state the builder reports the
  outcome as unknown and shows no charged total at all; `status` is what
  confirms the authoritative figure.

Tests cover both halves: an HTTP-level test asserts the service's totals
equal the pricing package's cent for cent, and an end-to-end test drives
the real builder through a fault-injecting proxy and then compares the
total it displayed with the total the service actually stored.

### Extensibility: menu and promos as data

Adding a **size** is one row in the size table (name, base price). Adding
a **topping** is one row (name, category). Adding a **topping category**
is one row (name, percentage of base); toppings in it are priced
automatically, because a topping's price is derived from its category's
rate and the pizza's base price — no topping carries its own price, and
no pricing code mentions any specific size, topping, or category.

A **promo code** is a record: its kind (percent-off or fixed-amount-off
the pre-tax subtotal), its parameters, an optional qualifying condition
(a minimum pre-tax subtotal, strictly greater — SAVE5's "over $20"), and
a list of codes it conflicts with. The engine interprets records
generically: compute the discount if it qualifies, floor a fixed discount
at the subtotal, and refuse conflicting combinations using the records'
own conflict lists (validated for symmetry by a test). A new promo whose
condition is a minimum subtotal and whose conflicts are named codes is a
new record, not a new branch — for example "$8 off over $50, never with
SAVE5":

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

That is one row in `promoList`; no pricing code changes, and the
conflict-symmetry test requires the matching entry on `SAVE5`. The core
pricing tests stay as they are — only the menu/promo regression tests
grow a case for the new record.

The honest limits of this design, since "data-driven" is easy to
overclaim:

- **New discount *mechanisms* need code.** "Cheapest pizza free", a
  category-specific discount, or a buy-one-get-one all require a new
  `PromoKind` and a case in the discount function. Only percent-off and
  fixed-amount-off the pre-tax subtotal are data today.
- **New *condition types* need code.** The only qualifying condition is a
  minimum pre-tax subtotal. A condition on item count, a specific size, a
  date range, or a customer attribute is a new field and new evaluation
  logic, not a new row.
- **Exclusion is pairwise and total.** Conflicts are declared as symmetric
  code lists and at most one promo applies per order. "Stacking up to two
  promos" or "this promo stacks only with that one" is a different model
  and would need a rework, not a record.
- **Promo application order is not modelled**, because only one promo can
  apply. Supporting several would force a decision about sequencing that
  the current design deliberately does not make.

Conditions and conflicts — the dimensions the promos in this spec
actually vary along — are pure data.

### Order lifecycle, retries, and races

An order moves `Not Started → Started → Ready for Pickup → Delivered`,
advanced only by the restaurant side; a customer can update or cancel it
only in `Not Started`. Every mutation checks the current status and
mutates while holding the store's single mutex, so each check-and-change
is atomic.

*Retry after a timeout.* The builder generates one idempotency key per
order and sends it with the submission. If the request times out — the
order may or may not have arrived — the builder retries once with the
**same** key and tells the customer it is doing so. The service records
key → order ID on first submission; the retry finds the key and returns
the original order (HTTP 200 instead of 201) instead of creating a
duplicate. Guarantee: at most one order created per key within the
running service.

*Ambiguous outcomes.* The builder keeps the key and the exact submitted
contents whenever it cannot prove the order was **not** created. In that
state it refuses local edits (so the key never comes to mean two
different orders), explains the situation, and `submit` resends the
original snapshot under the original key.

The key is released only for a response that *establishes* non-creation,
which is narrower than "any 4xx":

| Response | Treated as | Why |
| --- | --- | --- |
| 201, 200 | Resolved | Created, or an idempotent replay. |
| A service *validation* rejection (e.g. 400 on a bad promo) | Non-creation established | The service judges the contents before storing, so nothing exists. |
| 409 | Key is in use | Proof the key *was* used; it is kept and the original order ID is reported. |
| 408, 425 | Ambiguous | The request failed to arrive usably — a proxy can emit these after the origin stored the order. |
| 5xx | Ambiguous | May have been emitted after creation upstream. |
| Other 2xx (202, 204, …) | Ambiguous | Not a defined outcome; 202 means "queued, not necessarily stored". |
| Timeout / unreadable / contentless body | Ambiguous | The reply says nothing reliable. |

A 200 or 201 must also carry `id`, `status`, and `total_formatted` before
success is announced or the session's order ID is replaced.

The honest limits:

- **"Validation rejection" is inferred, not authenticated.** 408, 425,
  and 409 are singled out by status code, and everything else in `[400,
  500)` is *treated* as the service's own judgement on the contents. A
  4xx an intermediary synthesizes for its own reasons — a proxy's 403 or
  404 — is indistinguishable on the wire from that judgement, so the
  builder would release the key for it. Narrowing this further needs a
  signal the service controls that a proxy cannot forge, which this
  exercise does not build.
- **The key lives in memory**, so restarting the builder before an
  outcome is known loses it, and a later submission could then duplicate
  an order the service had already accepted.
- **No eventual delivery is promised** across a permanent network
  failure — only that retrying against the running service cannot create
  a second order.
- **`status` does not resolve anything.** `status <order-id>` looks up an
  order you already know the ID of; it deliberately does not adopt that
  ID as the session's order or resolve a pending submission. It also
  reports only the ID, lifecycle status, and charged total — never items,
  toppings, tip, or promo — so it cannot confirm *which* contents the
  service holds after an uncertain `PATCH`: two different orders can
  share a total. The builder says so in that situation and prints the
  `GET /orders/{id}` call that does show the stored contents. The surest
  recovery remains resending the same update, which is a wholesale
  replacement and therefore idempotent.

*Cancel at the moment the kitchen starts.* Both operations serialize on
the store mutex, and each validates the status inside the lock. One of
them runs first and succeeds; the other observes the new status —
`Started` blocks the cancel, `Cancelled` blocks the advance — and fails
with a 409 and a message naming the current status. Guarantee: exactly
one winner, never an order that is both cancelled and started. A test
releases a cancel and an advance simultaneously against the same order
100 times and asserts exactly one success and a consistent final state
every time (and the store is exercised under `go test -race`).

### What was cut, and what comes next

Deliberately cut to stay in the timebox: persistence (in-memory store
only); a real kitchen/delivery integration (advancement is an endpoint
or a timer); authentication; partial updates; concurrent editing aids
(order versioning/ETags — updates are last-writer-wins, acceptable for a
single anonymous customer per order); a menu administration UI (the menu
is a data table in code). Proportionate for a 3-hour exercise; each is a
documented assumption rather than a hidden gap.

Next, in order:

1. **Persist orders and idempotency keys.** This is the one cut with a
   correctness consequence rather than only a convenience cost: because
   the key→order record lives in memory, a service restart forgets it, so
   a retry after a restart is no longer protected and can create a second
   order. The store's operations map onto a single SQL table plus a
   uniqueness constraint on the key, and the lock discipline becomes a
   transaction — but note this is a *substitution*, not a drop-in: `Store`
   is a concrete type today, so persisting it means either introducing an
   interface or changing that type, and every method would start
   returning errors the handlers must map.
2. **Unguessable order IDs and auth on the restaurant-side endpoints.**
   IDs are currently sequential (`ord-0001`), so anyone can enumerate and
   cancel other customers' orders; `/advance` is likewise unauthenticated.
   Acceptable only because the exercise excludes auth.
3. **Order versioning (ETag / `If-Match`)** so an update based on stale
   state gets a 409 instead of silently overwriting. Today updates are
   last-writer-wins, which is fine for one customer per order and wrong
   the moment two devices share one.
4. **Structured request logging and a metrics hook** (order counts by
   status, rejection reasons) — the operability a real service is paged
   on, and what would reveal whether the ambiguous-outcome paths above
   ever actually fire in production.

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

## AI usage note

**Tools used.** Claude (Anthropic) and GitHub Copilot were both used on
this repository: Claude produced the first working implementation —
structure, pricing/service/builder code, and tests — from the exercise
specification, and both were used across the subsequent review rounds that
produced the audit in `TODO.md` and the fixes recorded there. The commit
history shows those rounds as separate commits rather than one drop.

The design decisions in the Assumptions section (integer-cents money, a
shared pricing package as the single source of truth, idempotency keys,
the locked state machine, the tip-base and order-of-operations choices)
were specified up front and then checked against the AI's output.

An important caveat about this section, stated plainly because it affects
how the rest of it should be read: the specification audits and the
fix rounds were themselves AI-assisted. Where a finding below is
described as "reproduced" or "verified", that means it was demonstrated
with a running program and is now pinned by a test anyone can re-run —
not that it was found by unaided human inspection. The distinction
matters, so it is drawn rather than blurred.

- **One example of AI output that was rejected and fixed.** The initial
  API client discarded JSON decode errors (the `json.Decode` error was
  assigned to `_`), so an empty or truncated HTTP 201 response was
  reported as a successful submission and printed
  `Order  submitted. Status: . Total charged: .`. Two rounds of fixes
  were needed, because the first round was incomplete:
  1. `apiClient.call` now returns body-read and decode errors, wrapping
     them with `%w` so a body-read timeout is still classified as a
     timeout and the retry branch fires.
  2. That alone was not enough: a well-formed but contentless reply
     (`{}` or `null`) still decoded cleanly and was announced as success.
     `orderResponse.validateOrder` now requires `id`, `status`, and
     `total_formatted` before any success message is printed or the
     session's order ID is replaced.

  A related correction in the same area, which took three passes to get
  right and is the clearest example of AI output that looked finished but
  was not. The original code assumed a non-timeout error proved the
  request never reached the service, and discarded the idempotency key —
  so a retry created a duplicate. The first fix narrowed that to "release
  the key on any 4xx", which is still wrong: a 408 or 425 reports that the
  request failed to *arrive usably* and can be emitted by a proxy after
  the origin already stored the order. The rule is now that the key is
  released only for a response that reads as the service's own
  *validation* rejection of the contents, with 408/425, 409, 5xx, and any
  unexpected 2xx all treated as unresolved. That rule is still an
  inference from the status code rather than an authenticated signal —
  the residual gap is stated in the retry section above — but it no
  longer releases the key for the delivery failures that actually
  produced the duplicate. Each wrong version passed the tests that
  existed when it was written, which is the lesson.

- **One invariant checked by demonstration rather than by trusting the
  code.** The invariant is requirement 5's: *an unreliable connection must
  never leave the restaurant holding two orders.* Reading the code was not
  enough to settle it — the code looked correct in all three of the wrong
  versions above. So it is checked by construction instead, in
  `cmd/builder/recovery_test.go`: the real builder talks to the real
  service through a proxy that always delivers the request and then
  damages only the reply. For each of six failure modes (lost body,
  truncated JSON, 502, a 408 issued after the order was stored, a
  contentless 200, and a 202) the test retries and then asserts directly
  against the service's own store that **exactly one** order exists and
  that the ID the customer holds is that order.

  That construction is what makes the claim falsifiable: reverting any of
  the fixes makes `store.Count()` return 2 and the test fail by naming the
  duplicate. The same method found the other defects — the false-success
  decoding, the dollar-conversion wrap (`tip amount 184467440737095517`
  displaying `$0.84`), the replay-comparison inversion after a `PATCH`, and
  the `float64` round-trip that turned `quantity:1.00000000000000001` into
  quantity 1 — each of which the test suite of the time accepted. Every
  one is now pinned by a regression test, so all the claims in this
  section can be re-checked with `go test -race ./...`.

> **Reviewer's note.** Treat this section as a description of work that is
> inspectable in the repository — the commits, the fixes, and the tests
> named above — rather than as a claim about unaided human effort. The
> audits and fixes were AI-assisted, which is precisely why each finding
> is tied to a test that fails when the fix is reverted: the evidence
> stands on being reproducible, not on who or what noticed it first.

## Repository layout

```
internal/pricing/   menu data, promo records, and the one Price function
internal/order/     order model and lifecycle statuses
internal/store/     in-memory store: idempotency + locked state machine
internal/server/    HTTP handlers (thin) over the store
cmd/orderservice/   the order service binary
cmd/builder/        the interactive order builder CLI
```
