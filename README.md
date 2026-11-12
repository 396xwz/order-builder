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

Building binaries instead works too: `go build ./...` produces
`orderservice` and `builder`.

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
totals field: the service computes totals itself and ignores nothing
because there is nothing to ignore.

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
   $20-or-less order (at set time, submit, or update) is an error with a
   clear message. The customer is never silently denied a discount they
   think they have — nor silently charged without one.
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
charges" — is therefore guaranteed by construction rather than by keeping
two implementations in sync, and a test submits an order over HTTP and
asserts the service's totals equal the pricing package's, cent for cent.

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
duplicate. Guarantee: at most one order per key, and the customer ends up
holding the right ID either way.

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

Run with `go test ./...` (the store and its race test are also clean
under `go test -race ./...`).

Invariants chosen, and why:

- **Pricing arithmetic is exact.** Every size × topping-category price;
  three full orders computed by hand from the spec, including rounding
  cases (a 92.82¢ tax, a 201.2¢ discount). Pricing is the heart of the
  exercise, so it gets the deepest coverage.
- **The SAVE5 boundary.** Exactly $20.00 does not qualify; $20.01 does.
  "Over $20" is precisely the kind of phrase implementations get wrong.
- **Promo mutual exclusion** (including symmetry of the conflict data, so
  exclusion cannot depend on which code was applied first).
- **Builder total == charged total**, asserted over HTTP.
- **Idempotent resubmission** returns the original order and stores
  exactly one — at the store level and over HTTP.
- **Illegal lifecycle transitions are rejected**: update/cancel after
  Started, advancing a Delivered or Cancelled order.
- **The cancel-vs-advance race** yields exactly one winner (100 rounds).
- **Invalid input is rejected with errors**: unknown size/topping/promo,
  zero quantity, duplicate topping, out-of-range tip.

Not covered: the CLI's interactive loop itself (its pricing and API paths
are covered through the packages it calls; the loop was verified by
scripted end-to-end sessions against the real service), load/performance,
and behavior across service restarts (storage is documented as
in-memory).

## AI usage note

An AI coding assistant (Muse) was used to produce the first working
version of this implementation — structure, pricing/service/builder
code, and tests — working from the exercise specification. The design
decisions in the Assumptions section (integer-cents money, a shared
pricing package as the single source of truth, idempotency keys, the
locked state machine, the tip-base and order-of-operations choices) were
specified up front and then checked against the AI's output.

- One example of AI output I rejected or had to fix: the initial API
  client silently discarded JSON decode errors (`json.Decode` error was
  assigned to `_`), so an empty or truncated HTTP 201 response was
  reported as a successful submission. I changed the client to return
  body-read and decode errors, and added focused tests for empty body,
  malformed JSON, and body-timeout scenarios. This ensures requirement
  5's unreliable-network scenario is handled honestly rather than
  displaying an empty order status.
- One behavior or invariant I verified myself rather than trusting the
  AI, and how: I performed a complete specification audit comparing the
  implementation against all four pages of the exercise PDF, the README,
  both commands, all internal packages, and their tests. This uncovered
  three correctness issues (response decoding, unstable submission
  snapshots, and unchecked integer overflow) that the existing test suite
  did not catch. I also hand-checked the pizza pricing example from the
  README and confirmed the numbers matched when the annotation's
  additional two Small pizzas were included.

## Repository layout

```
internal/pricing/   menu data, promo records, and the one Price function
internal/order/     order model and lifecycle statuses
internal/store/     in-memory store: idempotency + locked state machine
internal/server/    HTTP handlers (thin) over the store
cmd/orderservice/   the order service binary
cmd/builder/        the interactive order builder CLI
```
