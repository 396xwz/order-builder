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

## Design write-up

### Money and the single source of truth for pricing

Money is an integer number of cents (`pricing.Cents`) everywhere —
never a float — and is formatted to dollars only when displayed. All
pricing lives in one package, `internal/pricing`: the menu (sizes,
topping categories, toppings) as data tables, promo codes as data records
(see below), and a single `Price` function that computes the breakdown in
the documented order of operations.

Pricing runs in **both** processes, from the **same** package. The
builder computes the running total locally so the customer sees it update
on every edit with no network round-trips. The service recomputes the
total on every submission and update and never accepts a client-supplied
total (the request format has no totals field at all). Requirement 2 —
"the total the customer sees must match the total the restaurant
charges" — is therefore guaranteed for the same input on both sides:
shared pricing prevents calculation drift, but the guarantee also
depends on the client sending the correct contents (verifiable by the
server's strict JSON decoding; see F01/F07) and the response body being
correctly received (verifiable by the client's response decode error
handling; see F01). An HTTP-level test asserts the service's totals
equal the pricing package's, cent for cent, for a round-tripped order.

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
own conflict lists (validated for symmetry by a test). A new promo —
including one with its own conditions and conflicts — is a new record,
not a new branch. The honest limit of this design: a promo whose
*mechanism* is new (say, "cheapest pizza free", or a discount on a
single category) needs a new `PromoKind` and a case in the discount
function. Conditions and conflicts, the dimensions promos in this spec
actually vary along, are pure data.

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
contents whenever it cannot prove the order was **not** created — a
timeout, an unreadable or contentless reply, or a 5xx that may have
followed creation upstream. In that state it refuses local edits (so the
key never comes to mean two different orders), explains the situation,
and `submit` resends the original snapshot under the original key. The
key is released only on a 4xx validation rejection, which the service
emits before storing anything; a 409 means the key is already taken, so
it is kept and the original order ID is reported. A 2xx reply must
contain `id`, `status`, and `total_formatted` before success is
announced or the session's order ID is replaced.

The honest limits: the key lives in memory, so restarting the builder
before an outcome is known loses it, and a later submission could then
duplicate an order the service had already accepted. Nothing here
promises eventual delivery across a permanent network failure — only
that retrying against the running service cannot create a second order.
`status <order-id>` looks up an order you already know the ID of; it
deliberately does not adopt that ID as the session's order or resolve a
pending submission.

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

Next, in order: (1) persist orders and idempotency keys (SQLite drops in
behind the store's interface unchanged); (2) unguessable order IDs and
auth on the restaurant-side endpoints; (3) order versioning so an update
based on stale state gets a 409 instead of overwriting; (4) structured
request logging and a small metrics hook (order counts by status) — the
kind of operability a real service would be paged on.

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
- `Cents.String` handles the `MinInt64` negation edge case.

### `internal/order` — model and lifecycle

- Every `Status.Next` outcome and terminal-state detection.
- Deep-copy isolation.

### `internal/store` — lifecycle enforcement, idempotency, concurrency

- Update/cancel rejection for every non-`Not Started` status (Started,
  Ready for Pickup, Delivered, Cancelled).
- Atomic validation failure: an invalid update leaves the order's items,
  tip, totals, and timestamps unchanged.
- Store ownership: mutating a returned copy cannot mutate the stored
  order's line items or toppings.
- Idempotent create (same key → original order, no duplicate), including
  concurrent idempotency (10 goroutines with one key still produce one
  order and one original ID).
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
  terminal states, and returns the right count (including an empty
  store).
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
- Tip mode/field contradictions are rejected with 400.
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
- `cancel`/`status` use the session's order by default and an explicit
  ID when given; an explicit ID does not become the session's order, and
  with no ID and nothing submitted they make no request.

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
- A 4xx validation rejection releases the key and re-enables editing; a
  409 keeps the key and surfaces the original order ID.
- A timeout triggers exactly one automatic retry with the same key, and
  leaves the submission pending if that also fails.
- `submit` on an already-submitted order makes no request and directs
  the customer to `update`; an empty order makes no request at all.
- `status` and `cancel` refuse to confirm anything from a contentless
  reply, and a 502 with an HTML body is not reported as "unreachable".

Not covered: the CLI's interactive read loop itself (`main`'s scanner —
the commands it dispatches are tested directly), load/performance, and
behavior across service restarts (storage is documented as in-memory).
The scripted end-to-end sessions against a real service were a manual
check during development, not an automated guarantee.

## AI usage note

> _Attribution and the personal-verification example below must be
> confirmed by the candidate before submission — see the note at the end
> of this section. Everything else here describes work that is visible in
> the repository._

AI coding assistants were used to produce the first working version of
this implementation — structure, pricing/service/builder code, and tests
— working from the exercise specification, and then to carry out a
specification audit and the follow-up fixes. The design decisions in the
Assumptions section (integer-cents money, a shared pricing package as the
single source of truth, idempotency keys, the locked state machine, the
tip-base and order-of-operations choices) were specified up front and
then checked against the AI's output.

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

  A related correction in the same area: the original fix assumed a
  non-timeout error proved the request never reached the service, and so
  discarded the idempotency key. That is false — an empty 201 body or a
  5xx from an intermediary can follow creation upstream — and it caused
  duplicate orders. The key is now released only for a 4xx validation
  rejection, which is the one response that proves nothing was stored.

- **One invariant verified rather than trusted, and how.** A
  specification audit compared the implementation against all four pages
  of the exercise PDF, the README, both commands, every internal
  package, and the tests. It reproduced behavior with in-process servers
  and loopback proxies rather than reading code alone, which is how the
  false-success decoding, the key-discarding duplicate, the
  dollar-conversion wrap (`tip amount 184467440737095517` displaying
  `$0.84`), and the replay-comparison inversion after a `PATCH` were
  found — none of which the then-current test suite caught. Each
  finding is now pinned by a regression test in `cmd/builder/client_test.go`,
  `cmd/builder/main_test.go`, and `internal/store/store_test.go`, so the
  claims above can be re-checked with `go test -race ./...`.

> **For the candidate to confirm before submission:** name the specific
> assistants actually used (this repository's history involves more than
> one), and either endorse the verification account above as your own
> work or replace it with a check you performed yourself. An AI-produced
> audit is evidence, but it is not independent human verification, and
> the PDF asks for the latter.

## Repository layout

```
internal/pricing/   menu data, promo records, and the one Price function
internal/order/     order model and lifecycle statuses
internal/store/     in-memory store: idempotency + locked state machine
internal/server/    HTTP handlers (thin) over the store
cmd/orderservice/   the order service binary
cmd/builder/        the interactive order builder CLI
```
