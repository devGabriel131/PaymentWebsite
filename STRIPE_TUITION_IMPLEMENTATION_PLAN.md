# Stripe tuition implementation — hosted Checkout and scheduled invoices

## Context

Implement real student-specific Stripe-hosted Checkout, automatic collection of the existing finite tuition plans, durable payment history/balances, and signed-agreement nonpayment rules. User selected cards plus Apple Pay/Google Pay, continued installment collection during suspension, staff-initiated reinstatement with automatic reactivation after settlement, and a separate later dashboard task. Add only the operational billing section/actions needed in existing student details; no reporting dashboard or student account system. School removal here means `students.is_active=false` plus visible staff status; no external learning platform is connected.

## Grounded decisions

- `internal/payments/plans.go` remains the pure amount/calendar registry: `full=[60000]`, `three=[20834,20833,20833]`, `six=[10835,10833,10833,10833,10833,10833]`, all USD cents. Reuse `Find`, `Plan.TotalCents`, `Plan.Schedule`, `Money`, and `SpanishDate`.
- Use one-time Checkout for the first amount and **scheduled one-off Stripe invoices** for the remaining amounts. This supersedes the earlier subscription-schedule recommendation: [scheduled finalization](https://docs.stripe.com/invoicing/scheduled-finalization) supports exact future timestamps up to five years; [automatic collection](https://docs.stripe.com/invoicing/automatic-collection) supports card retries. No recurring object, prorations, indefinite-renewal window, or raw PaymentIntent renewal loop is needed.
- `Student.ID` is the durable enrollment identity. It is unique per submitted source link, not per person/email. Links remain bearer credentials with seven-day post-submit expiry; billing survives that expiry. Do not introduce email/phone deduplication or extend customer access.
- Current plan submission only calls `links.Store.ChoosePlan`; it does not charge. `Student.IsActive` currently has no production reader or status UI. Existing reusable patterns: explicit injected stores, `students.UpdateField` short transaction/row lock, `auth.Staff`, post-commit `feed.Changed`, and `testdb.Open` isolated schemas.
- Stripe MCP is not exposed (`read mcp://` reported no manager), and `command -v stripe` exited 1. Account, sandbox, retry settings, catalog and tax status are unverified. No application, account or system mutations were performed during planning.

## Approach

Steps 1–3 add the billing backend without changing current customer routes. Cut over routes and their callers/tests atomically in step 4; step 5 completes operator workflows. Run verification after the complete change, not builds/tests between edits.

### 1. Establish Stripe configuration and durable financial ownership

Add `github.com/stripe/stripe-go/v87@v87.0.0` to `go.mod`/`go.sum` using `go get github.com/stripe/stripe-go/v87@v87.0.0`; this [stable release](https://github.com/stripe/stripe-go/releases/tag/v87.0.0) and [current client pattern](https://github.com/stripe/stripe-go/blob/master/README.md) were verified. Use `stripe.NewClient`, API `2026-09-30.endive`, and instance services only. No module-global API key, custom HMAC, extra job infrastructure, or provider interface hierarchy.

Create `internal/billing/` with `models.go` (persistence), `billing.go` (commands/read models), `stripe.go` (SDK operations/catalog), `worker.go` (reconciliation/school deadlines), and `webhook.go` (signed event ingress). No equivalent provider integration exists. Keep `internal/payments` and the existing stores otherwise intact.

Add these exact configuration names, loaded in server mode only after the existing staff-CLI branch:

- `STRIPE_API_KEY`: minimum-permission restricted application key, never printed.
- `STRIPE_WEBHOOK_SECRET`: this environment's signing secret; CLI-forwarded secrets differ from registered endpoint secrets.
- `STRIPE_MODE`: exactly `sandbox` or `live`; reject incompatible key prefixes and Stripe resource/event `livemode`.
- `PUBLIC_BASE_URL`: trusted absolute origin, no userinfo/query/fragment/path prefix. HTTPS required except localhost HTTP in sandbox. Never build Stripe return URLs from request Host/forwarded headers.
- `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID`: configuration exposing cards and supported Apple Pay/Google Pay wallets, not bank debit/BNPL/other non-card methods for this launch.
- `STRIPE_PORTAL_CONFIGURATION_ID`: existing Stripe-hosted portal configuration for invoice history and saved-card updates only.

Missing/invalid server configuration fails startup with the existing safe startup boundary. `user` CLI and `--help` must still work without Stripe configuration. Create SDK backends with an HTTP timeout of 5 seconds, at most the SDK's two network retries, and `LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}` through `stripe.NewBackendsWithConfig`; preserve request context deadlines. This prevents SDK raw errors/payloads from bypassing project logging privacy.

Use one batched Price list lookup (six keys, below Stripe's limit of ten), not repeated Product/Price creation per payment. Load/validate the catalog before serving under a fresh fifteen-second startup context, not the earlier DB-migration timeout: every key must resolve once, active, correct `livemode`, USD, fixed `per_unit`, **one-time** price with expected cents. Missing, conflicting, wrong-currency or wrong-amount configuration is an error, never a fallback price. Independently pin each Checkout's `currency=usd`; manual Price `currency_options` can otherwise localize even when Adaptive Pricing is off.

| Lookup key | Product | Unit cents |
| --- | --- | ---: |
| `asvab_full_initial_usd` | Pago Único | 60000 |
| `asvab_three_initial_usd` | Plan de 3 Pagos Mensuales | 20834 |
| `asvab_three_remaining_usd` | Same three-payment Product | 20833 |
| `asvab_six_initial_usd` | Plan de 6 Pagos Mensuales | 10835 |
| `asvab_six_remaining_usd` | Same six-payment Product | 10833 |
| `asvab_reinstatement_usd` | Reinstalación | 2500 |

Derive expected tuition cents from the existing registry, not a second amount registry. Reinstatement's 2500-cent business constant belongs in billing. Each tuition option gets its own Product; first/later prices are variants of that Product. See [price lookup keys](https://docs.stripe.com/api/prices/list?query=lookup_keys).

Define the following GORM models. Internal IDs are `uint64`/SQL BIGINT, except explicitly named Stripe primary IDs. Foreign keys use RESTRICT; financial history has no soft deletion. Money stays integer cents/SQL BIGINT; validate provider amounts against the bounded tuition/fee domain before conversion to the existing `Money(int)` representation. Common fields are `CreatedAt`, `UpdatedAt`; nullable provider IDs use `*string`, not unique empty strings.

| Model/table | Required owned data and constraints |
| --- | --- |
| `Contract` / `billing_contracts` | Unique `StudentID` FK; random unique `Reference` from `crypto/rand.Text()`; frozen `PlanID`, `PlanName`, `InstallmentCount`, `Currency="usd"`, `TotalCents`; nullable `FundedAt`; unique nullable `StripeCustomerID`; current checkout attempt reference; setup `AttentionReason`; `LastSyncedAt`, `NextSyncAt`, lease token/deadline. One enrollment owns one contract. |
| `Obligation` / `billing_obligations` | `ContractID`; random unique `Reference`; `Kind` exactly `tuition` or `reinstatement`; `Number`; positive `AmountCents`; immutable selected `StripePriceID`; nullable `DueAt` until funding; nullable unique `SuspensionID` for fees; indexed nullable `DeadlineEpisodeID` for tuition deadline attribution; `CollectionDisposition` exactly `collect` or `cancel` (tuition remains collect); unique nullable `StripeInvoiceID` and `StripeInvoiceItemID`; invoice/item creation-start timestamps; verified scheduled-finalization timestamp; latest Stripe status, amount paid/remaining/other settlement, immutable earliest `FirstSettledAt`, failure/action-required state and its own `ReviewReason`. Unique `(contract_id,number)` for tuition; fee obligations have one `SuspensionID` each. |
| `CheckoutAttempt` / `billing_checkout_attempts` | `ContractID`; random unique `Reference`; quoted `PlanID`, first amount, price ID; stable integration label and request parameters; creation-start time; nullable unique `StripeSessionID` and `StripePaymentIntentID`; Checkout URL and expiry; state exactly `creating`, `open`, `processing`, `expiring`, `uncertain`, `expired`, `failed`, or `paid`. Partial unique index on `contract_id` for the five nonterminal states prevents competing payable attempts. |
| `Payment` / `billing_payments` | Stripe PaymentIntent ID primary key; `ContractID`, unique nullable successful Charge ID, currency, gross received/refunded cents, payment time, dispute/fraud review state; nullable BalanceTransaction ID and actual fee/net cents. Processing/failed intents are not received money. Unknown fee/net is NULL, never zero by assumption. |
| `Allocation` / `billing_allocations` | Stripe InvoicePayment ID primary key; `ObligationID`, optional `PaymentID`, allocation status/type and cents. Keep allocations separate: one PaymentIntent can pay multiple invoices. Cash totals count Payment once, not each webhook/allocation. |
| `Suspension` / `billing_suspensions` | `ContractID`, triggering tuition installment number, its 14-day deadline, effective `StartedAt` (that deadline), actual `DetectedAt`; nullable `ReinstatementRequestedAt`, `RequestedBy`; nullable `ClosedAt` and nullable `CloseReason` (`reinstated` or `corrected`, present only when closed). Partial unique index on `contract_id WHERE closed_at IS NULL`; fee linkage is the obligation's `SuspensionID`. Tuition's `DeadlineEpisodeID` records which episode already handled a missed deadline, preventing repeated fees after reinstatement. |
| `WebhookEvent` / `billing_webhook_events` | Stripe event ID primary key; allowlisted type, resource ID, API version, live flag, received time; processed time, retry time/count, lease token/deadline and bounded error classification. Store no raw payload, headers, credentials, contact values or URLs. Index due unprocessed events. |

Add `billing.Models() []any`; extend the one `main.go` AutoMigrate list and the affected test fixtures with these models. Use GORM check/unique/partial-index tags, following existing model-owned schema convention. No framework, reset, enrollment backfill or invented historical payments. Existing local plan choices remain preselected preferences until checkout starts; students with no billing contract have no inferred Stripe history.

Expose these APIs; implement other helpers privately:

```go
type Config struct {
    BaseURL, WebhookSecret, PaymentMethodConfigurationID, PortalConfigurationID string
    Live bool
}
type Catalog struct { Full, ThreeFirst, ThreeLater, SixFirst, SixLater, Reinstatement string }
func LoadCatalog(ctx context.Context, client *stripe.Client, live bool) (Catalog, error)
func New(db *gorm.DB, client *stripe.Client, cfg Config, catalog Catalog, changed func(), log *slog.Logger) (*Service, error)
func Models() []any
func (s *Service) StartCheckout(ctx context.Context, sourceLinkHash []byte, planID string) (CheckoutResult, error)
func (s *Service) ForLink(ctx context.Context, sourceLinkHash []byte) (*Summary, error)
func (s *Service) ForStudent(ctx context.Context, studentID uint64) (*Summary, error)
func (s *Service) ReturnStatus(ctx context.Context, sessionID string) (ReturnStatus, error)
func (s *Service) RequestReinstatement(ctx context.Context, studentID uint64, staff string) (*Summary, error)
func (s *Service) InvoiceURL(ctx context.Context, studentID, obligationID uint64) (string, error)
func (s *Service) PortalURL(ctx context.Context, studentID uint64, staff string) (string, error)
func (s *Service) ReconcileStudent(ctx context.Context, studentID uint64, staff string) (*Summary, error)
func (s *Service) Webhook(w http.ResponseWriter, r *http.Request)
func (s *Service) Run(ctx context.Context)
```

`CheckoutResult` has `URL string`, `Pending bool`, `Funded bool`. `Summary` exposes frozen plan/quoted total, funding state, Stripe cash paid, other settlement, remaining tuition, currently overdue tuition, reinstatement fee remaining, actual due dates/history, student active/suspended state, last sync and the union of current setup/invoice/payment review reasons. One resolved issue must not erase another. `ReturnStatus` contains only payment state and amount, never enrollment identity/contact/history. No contract means nil Summary, not zero-valued paid status. Define `ErrUnavailable` (404), `ErrPlanLocked`, `ErrPaymentPending`, `ErrNotSuspended`, `ErrNeedsReview`, `ErrInvoiceNotReady` (409), and `ErrStaffRequired` (internal missing-attribution failure; normal unauthenticated requests are stopped by existing middleware); reuse `payments.ErrUnknownPlan` (422). Bounded review reasons are `unresolved_provider_create`, `invoice_mismatch`, `missing_invoice`, `payment_review`, `unsupported_payment_method`, and `configuration_mismatch`; transient provider failure is retry bookkeeping/last-sync staleness, not automatic debt forgiveness.

### 2. Create or resume exactly one hosted first-payment attempt

`StartCheckout` takes the server-resolved source digest, never browser student/customer IDs or amounts. Require its 32-byte length, then revalidate the matching Link's uncanceled/unexpired status, submitted contact, persisted consent, and Student inside a short transaction. Reuse the digest supplied by existing `links.Store.Lookup`; do not duplicate token hashing. Lock Link before Contract; financial status mutations lock Contract before Student, and the worker never locks Link. Read canonical student details without holding a Student row lock across a Contract wait.

Create the contract and its N quoted tuition obligations atomically; store exact cents/price IDs now, but leave actual dates unset until successful funding. Repeated/concurrent same-plan POSTs reuse the current attempt. Persist attempt identity and immutable request values **before** external calls, release the transaction, then call Stripe. A failed commit cannot initiate payment.

Create/reuse the enrollment's Stripe Customer using canonical student name/email, preferred locale `es`, and metadata `tuition_contract=<Contract.Reference>`. Persist its ID before Checkout creation. Different cardholder/email does not select another student. Never copy Stripe billing name/email back into canonical Student or Link snapshots.

Create Checkout through `client.V1CheckoutSessions.Create` with these wire fields:

- `mode=payment`, `currency=usd`; hosted UI; existing `customer`; one initial Price, quantity 1. Persist this currency in the immutable retry request.
- `invoice_creation.enabled=true` and invoice metadata `tuition_contract`, `tuition_obligation`, `tuition_attempt` containing only opaque server-created references.
- For three/six only: `payment_intent_data.setup_future_usage=off_session` and equivalent safe attempt/contract metadata. Do not save a card for the full-payment option without a future obligation.
- `payment_method_configuration=<configured ID>`; omit `payment_method_types` everywhere. `adaptive_pricing.enabled=false`, no promotion codes, adjustable quantities, discounts, tax or shipping additions.
- `client_reference_id=<Attempt.Reference>`; `locale=es`; `integration_identifier=tuition_<8 random letters>` generated once and stored per attempt.
- `success_url=<PUBLIC_BASE_URL>/payments/return?session_id={CHECKOUT_SESSION_ID}`; `cancel_url=<PUBLIC_BASE_URL>/payments/return?cancelled=1`. Never send the source bearer token, student contact or management session to Stripe in return URLs/metadata.
- Checkout text explicitly discloses first amount, remaining automatic installments, and that dates anchor to completed initial payment; existing selection page continues showing the full exact schedule preview and consent.

Verify returned currency/amount/customer/mode before exposing its URL. Use stable per-operation keys derived from opaque references, e.g. `tuition:<reference>:customer`, `tuition:<attempt-reference>:checkout`. Do not regenerate price, return URL, label or other request values during retries. Budget synchronous checkout work to eight seconds; timeout/disconnect/5xx means `uncertain`/Pending, not failed. Worker resumes/reconciles the same attempt.

Preserve prepayment plan changes safely. Before funding, a different selection must claim the contract, expire an existing **open** Checkout Session through `V1CheckoutSessions.Expire`, and confirm terminal unpaid/expired state with no processing/successful associated payment before changing the quote. Do not directly cancel a Checkout-managed PaymentIntent: Stripe requires Session expiry except for manual-capture cases this app does not create. Never unlock a completed/processing/uncertain payment; a paid Session awaiting invoice linkage remains processing until atomic funding. A cancel-page visit alone does not expire Checkout. After funding, a different plan returns `ErrPlanLocked`; same-plan replay displays status without another charge.

Stripe v1 keys may be pruned after 24 hours. Store all provider IDs immediately. For an uncertain create older than 23 hours with no ID, reconcile using paginated resource lists, known Customer where available, and the exact opaque server reference; do not use eventually consistent Search to prove absence. A unique validated match can be attached. Zero/multiple matches stay `AttentionReason=unresolved_provider_create` and block fresh creates; staff reconciliation must resolve this in Stripe rather than risk a duplicate charge. This is an explicit safety stop, not a fallback payment.

### 3. Fund once, pre-schedule remaining invoices, and reconcile durable events

Register `POST /webhooks/stripe` on the root mux, outside staff authentication. Retain native cross-origin protection; normal Stripe server-to-server POSTs pass without a broad bypass. Read at most 1 MiB raw body, verify with `webhook.ConstructEvent` and the normal five-minute tolerance/API-version check, then verify mode. Signature/JSON/version/mode failures return 400, oversized body 413. An allowlisted valid event is inserted by unique event ID; return 200 only after durable insertion or confirmed duplicate. DB failure returns 500 for Stripe retry. A valid irrelevant event returns 200 without work. Never log body/signature/secret or suppress verification errors.

Subscribe to exactly the events acted on:

- `checkout.session.completed`, `checkout.session.async_payment_succeeded`, `checkout.session.async_payment_failed`, `checkout.session.expired`.
- `invoice.created`, `invoice.finalized`, `invoice.finalization_failed`, `invoice.paid`, `invoice.payment_failed`, `invoice.payment_action_required`, `invoice.updated`, `invoice.voided`, `invoice.marked_uncollectible`, `invoice.deleted`.
- `charge.updated`, `charge.refunded`, `refund.updated`, `charge.dispute.created`, `charge.dispute.updated`, `charge.dispute.closed`, `radar.early_fraud_warning.created`. Route Charge events through the mapped PaymentIntent; asynchronous capture can supply `balance_transaction` only later in `charge.updated`.
- `customer.updated` to reconcile saved-method availability/billing configuration; canonical student contact fields remain application-owned.

Use snapshot events at API `2026-09-30.endive`. No subscription events are required because this design creates no subscriptions.

Worker claims due events/contracts with short `FOR UPDATE SKIP LOCKED` transactions and 60-second leases; provider reads/writes happen after commit. Renew the matching lease between persisted provider stages; a lost/expired lease stops further calls, and results apply only with its token and Contract lock. No two callers may run a contract workflow concurrently. Normal resource-ID mappings are authoritative. Only when recovering a partially persisted creation may opaque metadata references bind an unknown resource, and only after Customer, contract/obligation, mode, USD amount and line ownership are verified. A known deleted invoice persists `missing_invoice` review without a replacement. For an unmapped tombstone retrieve its original Event by stored event ID for ownership correlation; if unavailable, retain unresolved work without inventing an owner or charge. Unrelated account activity is ignored.

Read current Stripe objects rather than applying old webhook snapshots or ordering by event timestamps. On Checkout completion gate on `payment_status=paid`; `completed` with unpaid/processing status is not funding. Retrieve its generated paid invoice and all InvoicePayments, following pagination. Anchor `FundedAt` to **`invoice.status_transitions.paid_at`**, not event delivery, session creation or enrollment date. Validate exact initial amount. In one transaction store the initial receipt/allocation, set funding once, assign all dates, and only then make the attempt terminal `paid`. Build the date-generating `payments.Plan` from the contract's frozen ID/name and persisted tuition amounts ordered by Number; do not reload today's registry. A paid Session whose generated invoice/linkage is temporarily unavailable stays active `processing`, blocking another Checkout or quote replacement. Never invent a timestamp. See [paid invoice generation](https://docs.stripe.com/receipts#paid-invoice-receipts).

For installment plans retrieve the successful PaymentIntent's attached card; validate it belongs to the contract Customer. Set `Customer.invoice_settings.default_payment_method` explicitly before arming future invoices—Checkout's off-session setup attaches the method but does not set this default. This initialization runs once; on crash recovery retrieve the Customer and do not overwrite an already selected nonempty default. Future invoices must omit invoice-level `default_payment_method`, so later Stripe-hosted card updates apply.

For each remaining tuition obligation, create exactly one one-off invoice through these persisted stages:

1. `V1Invoices.Create`: matching Customer, `currency=usd`, `collection_method=charge_automatically`, `auto_advance=false`, `pending_invoice_items_behavior=exclude`, safe contract/obligation metadata. Persist invoice ID.
2. `V1InvoiceItems.Create`: explicitly target that invoice and Customer; use snapshotted one-time Price with `pricing.price`, quantity 1; Spanish description `Pago <n> de <N> — <plan name>`; safe obligation reference. Persist item ID.
3. Retrieve/validate exactly the intended line total/currency and no unexpected tax/discount/customer debit. Only then `V1Invoices.Update` with `auto_advance=true` and `automatically_finalizes_at=Obligation.DueAt.Unix()`. Persist the verified armed state.

Use a distinct deterministic idempotency key for each stage. A crash before step 3 leaves a noncollecting draft, not a zero-dollar paid invoice. Dates are the existing Puerto Rico local midnights (04:00 UTC); the original day is restored after short months. First payment is already collected: create **zero/two/five** later invoices for full/three/six, never another initial invoice or a repeating object.

If recovery happens after an agreed date, retain that original due date. Complete the same invoice and finalize it now with automatic advancement; do not move the customer's debt date or create a substitute invoice. A pending/paid existing invoice is reconciled, never manually recharged. Native Stripe retries the same open invoice; application does not implement a retry-charge loop. Finalization/collection has no documented exact-second guarantee; the UI promises calendar dates, not atomic midnight charge timing.

Reconciliation stores current invoice state, every paid allocation, each successful PaymentIntent once, current charge/refund/dispute state, and available actual balance-transaction fee/net. A credit/other settlement can reduce invoice balance without Stripe cash: expose it separately. Ready future drafts owe quoted cents; validated open/paid invoices use current amount remaining, with cash versus other settlement separate. Voided/uncollectible/deleted/mismatched resources, refunds and disputes never silently erase signed tuition or create replacement charges. Persist `ReviewReason` before acting; if an existing invoice has an unexpected total/currency/line, disable its `auto_advance` while unresolved, never re-arm it merely because its setup stage is incomplete, and do not offer its payment link. Preserve history and block false paid-in-full status. Refund/dispute handling does not automatically rebill refunded money or initiate refunds.

A five-second worker tick drains up to 25 due events then up to 10 due contracts; process one provider workflow at a time per claimed contract. Retry transient failures with capped exponential delay from five seconds to fifteen minutes. Reconcile processing/overdue contracts and received payments with incomplete balance-transaction/fee/net data at least every fifteen minutes. Audit **every funded contract, including fully settled ones**, daily: refresh invoices/payments/charges, refunds, and paginated disputes/early fraud warnings filtered by stored Charge or PaymentIntent, so missed post-completion events cannot remain invisible. Persist `NextSyncAt` no later than the nearest installment date or not-yet-handled 14-day deadline. Missing/deleted/edited objects become review cases, not replacement invoices. `// ponytail: daily per-contract audit suits small enrollment history; use an account event cursor before sustained volume.`

In `main.run`, derive a worker context from `signalContext`, start `Run(workerContext)`, and register cleanup that **cancels this child context on every server-exit path before waiting for the worker**. An unexpected `serve` return without an OS signal must not leave billing running or hang the SQL-pool cleanup. Signal cancellation stops claims/provider calls immediately; ordinary in-flight HTTP request contexts still retain `serve`'s ten-second drain. Wait for worker exit before closing SQL. Call `feed.Changed` only after committed visible financial/status changes; lease/retry bookkeeping and duplicate notifications must not trigger refresh storms.

### 4. Cut over customer payment routes without weakening enrollment access

Change exported registrars atomically:

```go
customer.RegisterRoutes(mux *http.ServeMux, store *links.Store, configuration *settings.Store, biller *billing.Service) error
management.RegisterRoutes(mux *http.ServeMux, store *links.Store, roster *students.Store, config *settings.Store, staff func(context.Context) string, biller *billing.Service) error
```

Customer callers: `main.go`, `internal/auth/http_test.go:newApp`, `internal/logging/http_integration_test.go:routes`, `internal/customer/customer_test.go:newMux`. Management callers: `main.go`, `internal/auth/http_test.go:newApp`, `internal/logging/http_integration_test.go:routes`, and all three registrations in `internal/management/management_test.go` (`newApp`, failed-DB fixture, failed-settings fixture). LSP references were consulted; its default build excludes integration-tagged callers and some returned line hints were stale, so reread these named fixtures before edits. Do not retain a nil-billing compatibility path.

Update every affected test fixture in the same cutover: customer package's repeated `testdb.Open` sites can share one `customerDatabase(t)` helper containing existing models plus `billing.Models()`. Extend `newMux`, `newAgreementFixture`, management `newApp`/failed-store fixtures, auth `newAuthDatabase`/`newApp`, and logging integration `routes` to construct a real billing service with an explicit test catalog/client and the complete schema. Do not couple generic `testdb.Open` to billing globally. Remove obsolete ChoosePlan-only tests from `links_test.go`, migrate behavioral isolation/gating/replay coverage to billing/customer tests, and delete wording/markup-only payment assertions rather than repinning changed copy. Keep exact-cent/calendar tests, contact/consent/expiry, HEAD, auth/CSRF, no-op/commit and shutdown contracts.

Split payment handling into new `internal/customer/payments.go`; leave consent handling in `agreement.go`. Retain current body limit, single allowlisted `payment_plan` parsing, 400/413/422 behavior, field errors and prior-selection distinction. Contact/consent and link usability still take precedence; unknown/canceled/expired links return branded 404. GET/HEAD must not create Stripe objects, payments or agreements.

`POST /customer/{token}/payments` calls `StartCheckout` with the looked-up digest. Ready returns 303 to Stripe (HTMX requests use existing `HX-Redirect` convention); pending/ambiguous provider outcomes return 202 with truthful Spanish processing/recheck UI; an unsafe plan change returns 409 showing the locked/pending plan, not success. A known pre-execution provider rejection/configuration error or DB failure returns retryable 500 without losing attempted selection; do not turn an ambiguous timeout/5xx into permission for a fresh attempt. Funded same-plan replay returns to its local status page. Expose a plain recheck link and resume action so no JavaScript is required. The background worker completes uncertain setup even if the browser disconnects.

Keep Link.PaymentPlan only as the existing pre-payment preference and update it inside billing's guarded transaction. Contract/obligation snapshots become financial authority. Remove `links.Store.ChoosePlan`, its obsolete comments, and the `internal/links` import of `payments` if now unused. Migrate its handler/test callers to billing behavior; no alias or second unguarded writer remains.

In `internal/customer/templates/payments.html`, replace save-only/no-charge copy and “Guardar plan” with **“Continuar al pago seguro”**. Before funding show all three complete preview calendars and explain automatic card use. Once funded show actual persisted plan, dates, paid/remaining/pending status; never show another initial-pay button. Processing is not paid; failed first attempts remain resumable/changeable only after terminal confirmation. Contact/consent text stays unchanged.

Add `GET /payments/return` and a `payment-return` template/view in the existing customer template shell. With `session_id`, read the persisted attempt only; show verified paid, pending, or not-completed status without student identity/contact/history. Unknown IDs return generic 404. `cancelled=1` shows not completed and explicitly does not mark payment failed or expire the session. No query parameter, return visit or HEAD mutates financial state. These token-free return pages can remain accessible after enrollment-link expiry without reopening enrollment.

Apply customer `no-store`, `no-referrer`, body-free HEAD and safe rendering to new responses. Add a customer CSP matching existing external styles/fonts/HTMX: self by default; explicit CDN HTMX and Google font/style origins; no inline scripts, objects or framing. Redirect-only Stripe integration requires no Stripe.js or permissive script/frame wildcards. Keep source tokens out of logs, Stripe metadata and return URLs.

### 5. Enforce school deadlines and give staff safe recovery actions

For funded enrollments, evaluate every tuition obligation once its persisted Puerto Rico `DueAt + 14 days` deadline passes. Refresh Stripe state before deciding; provider uncertainty postpones status changes and is retried visibly. Preserve earliest authoritative `FirstSettledAt`; full settlement at or before the deadline is on time. A currently paid invoice whose first full settlement was **after** the deadline still represents a valid missed deadline, even if a worker outage delayed detection. Record effective StartedAt and actual DetectedAt separately rather than inventing when the application actually updated the flag.

Under Contract then Student locks, attribute each genuine missed deadline to an existing non-corrected episode whose effective open-to-closed interval contains it, or to the one current open episode. Mark its `DeadlineEpisodeID` so retries and later audits do not suspend/charge again for an already handled late payment. Otherwise create one open episode at the earliest uncovered missed deadline and set `students.is_active=false`. Several late installments during the same continuous suspension incur only one fee. Keep all later invoices armed. Unfunded quotes start no deadline; unrelated inactive students without a billing episode are not automatically reactivated or assessed a fee.

Correct a falsely detected episode only from authoritative on-time settlement evidence. In the same locked decision, close it with `CloseReason=corrected`, release/reassign its tuition `DeadlineEpisodeID` attributions, and recompute **all** qualifying deadlines. Do not reactivate if another valid episode/deadline remains; establish or retain that valid episode atomically. A late notification about one installment cannot override another legitimate suspension.

Persist `CollectionDisposition=cancel` on the erroneous fee obligation before any remote cleanup, including when its create is uncertain with no invoice ID. Every resumed fee stage checks this intent. No provider create ever started → no fee resource is needed. Unknown create → reconcile solely to locate/cancel it, never finish/send it. Known draft → delete that draft; known finalized unpaid invoice → void it. An in-flight payment must first resolve; an already paid erroneous fee becomes staff refund review, never a new charge or automatic refund. A fully refunded canceled fee clears that fee's correction review and cannot count toward tuition/another episode's fee. Correction history and provider IDs remain retained.

`RequestReinstatement` rejects empty staff, absent student, active/non-billing-suspended student, or unresolved financial ownership with the specific errors above. Refresh original overdue invoices before creating a fee. Repeated requests reuse the episode and its single 2500-cent fee obligation, preserving first `RequestedBy`/request time. Create the fee as its own one-time `send_invoice` invoice with `days_until_due=1`, `auto_advance=false`, and `pending_invoice_items_behavior=exclude`; add its exact one-time fee Price and validate the total. Then call `V1Invoices.FinalizeInvoice` with `auto_advance=true` and `V1Invoices.SendInvoice`, each with a stable operation key and persisted stage. The one-day due window does not permit reinstatement before actual settlement. A send timeout must not generate another fee; after key retention expires, expose the already created invoice/portal link rather than automatically spamming fresh sends.

**Never put overdue tuition amounts on a new Checkout Session or fee invoice.** Original invoices may still retry automatically; duplicating debt creates double-charge risk. Student settles original overdue invoice(s) plus the separate fee through Stripe-hosted invoice history/pages. Automatically reactivate exactly once only when the requested episode's fee is confirmed settled, all currently due tuition is settled, and no unresolved payment/refund/dispute review remains. Late tuition payment alone after a valid suspension does not waive the fee or reactivate. Future dates remain unchanged; another later 14-day delinquency creates a new episode.

Extend `internal/management/students.go` and its existing template with a separate `student-billing` fragment/section: funding state, active/billing-suspended label, plan, tuition total, Stripe cash paid, other settlement, remaining and overdue tuition, fee remaining, due-date/invoice/payment history, last sync and attention state. Show no fabricated history for legacy records. Add Status to the existing list row for operational suspension visibility; no balance dashboard, charts, aggregate revenue or reporting filters.

Register under the already authenticated management mux:

| Route | Behavior |
| --- | --- |
| `GET /management/students/{id}/billing` | Read-only billing fragment, no provider writes. |
| `POST /management/students/{id}/billing/reconcile` | Staff-attributed reconciliation of existing resources, never permission for an unsafe fresh charge. |
| `POST /management/students/{id}/billing/reinstatement` | Idempotent staff request described above. |
| `GET /management/students/{id}/billing/invoices/{obligation}` | Verify obligation belongs to this student, retrieve current invoice URL and redirect to Stripe; draft/no URL returns an honest not-ready 409, ownership mismatch 404. |
| `POST /management/students/{id}/billing/portal` | Create a short-lived portal URL for the mapped Customer; display link for staff to share with intended payer, with no-store/no-referrer. Never accept a posted Stripe Customer ID. |

All POSTs use existing staff/session/origin checks, bounded form bodies, `auth.Staff`, and existing native/HTMX error conventions. Parse new student/obligation IDs with signed-BIGINT bounds (1 through `2^63-1`) before SQL. Missing IDs return 404, invalid state/review 409, known unexpected DB/provider failures 500; ambiguous financial writes remain durable pending state. Preserve current billing content and unsaved contact editors on failure. Add new POST paths to existing management auth/CSRF test path lists.

Portal configuration enables `features.invoice_history.enabled=true`, `features.payment_method_update.enabled=true` with the same card-method configuration; disable customer-identity edits, subscription changes/cancellation and email-only portal login. `PortalURL` uses `V1BillingPortalSessions.Create` with mapped Customer, fixed configuration, `locale=es`, and token-free base return URL. It does not mark paid/reactivate. This native saved-card update is necessary: paying one failed invoice with a different card does not necessarily change the Customer default for the next installment. No custom card-update form or new long-lived student auth is needed. See [portal integration](https://docs.stripe.com/customer-management/integrate-customer-portal).

Do not persist Hosted Invoice or portal URLs. Re-retrieve invoice URLs at the explicit invoice route, because [hosted links expire](https://docs.stripe.com/invoicing/hosted-invoice-page). A payer can continue through Stripe emails/pages after enrollment expiry; staff can issue a fresh mapped portal link. Load existing `management-live.js` on standalone details too and have only `student-billing` refresh on `management-changed`, preserving unsaved contact fields and modal/focus behavior. Reuse existing styles, `payments.Money` via template funcs and management time formatting; add no UI dependency or custom payment JavaScript.

Add logging operations `checkout_start`, `stripe_webhook`, `billing_reconcile`, `billing_reinstatement`, `billing_portal`, `billing_invoice_read`, `billing_worker` to `internal/logging/http.go`. Worker failures use existing `logging.ErrorAttrs` and bounded `billing_reconcile_failed`/`billing_review_required` messages with opaque internal reference/stage only. No raw SDK error text, URL, Stripe payload, card detail, contact value or secret.

### 6. Configure the real sandbox and prove the whole flow

Use the account's isolated development sandbox, not live mode or a shared test sandbox. If MCP is exposed, inspect account/sandbox settings with OAuth first. Otherwise install Stripe CLI using Bun (`bun add --global @stripe/cli@latest`), authenticate through its normal browser flow, and use `stripe whoami --format json`; never print config or request secret values in chat. Use a separately privileged sandbox setup credential for catalog/config/webhook creation; runtime key needs only Product/Price/config reads, Customer/Checkout/Invoice/InvoiceItem/portal-session writes and financial/event reads. Store secrets privately in platform secrets/environment; do not commit them.

Create or reuse the four Products/six Prices above by lookup key after validating existing values. Conflicting mode/amount definitions stop setup; do not silently transfer differently priced lookup keys. Configure cards/wallets dynamically in three distinct places: Checkout's selected configuration, the portal's update configuration, and the **Invoice default payment method configuration** for Hosted Invoice Pages. Disable Hosted Invoice Page Adaptive Pricing too, so later invoice presentment stays USD. Configure **one-off invoice** Smart Retries (eight tries within two weeks), failed-payment/authentication emails linking Stripe pages, successful receipts, no partial-payment custom amount and no underpayment-forgiveness threshold. Reuse merchant branding. No live charge or live setting change is authorized here.

Keep automatic tax disabled in sandbox and preserve quoted totals. Tax treatment/registrations require business confirmation before live mode; do not assume tuition exempt or enable automatic tax with no active registration. Processing, Invoicing and Checkout post-payment invoice fees are distinct; verify actual account pricing before live launch.

## Critical files & anchors

- `internal/customer/agreement.go:62-208` — combined consent/plan handler must split without bypassing current guards or private redirect behavior.
- `internal/links/links.go:303-330,403-420` — remove unguarded save-only writer; retain one existing token-digest/access implementation.
- `internal/students/students.go:19-25,68-109` — retained identity, status flag and row-lock/post-commit notification convention.
- `internal/management/templates/students.html:50-89` — current rows, modal/full detail and section boundary; refresh billing without replacing editors.
- `main.go:120-204` — current migration/wiring/lifetime, CLI-before-server setup and ten-second server drain.

Line hints are not stable; reread anchors before edits. These sources and all registrar fixture paths were read in planning.

## Verification

### Deterministic permanent regressions

Use real isolated PostgreSQL and the existing standard-library test style. A small SDK HTTP test server may inject uncertain/failure responses where essential; no mocking framework or copied request-echo assertions. Keep these named contracts in billing tests, integration-tagged when they use the DB:

1. `TestCheckoutConcurrencyAndRecovery`: concurrent/replayed starts produce one contract/one payable attempt; invalid or expired source/consent cannot start payment; commit failure starts none; provider-success/response-loss resumes same object; unknown older-than-key-retention stops safely; funded enrollment cannot pay first installment twice. Include paid Session plus temporarily unavailable generated invoice and a concurrent retry: attempt stays processing, no second Session. A multi-currency Price with a foreign-location test customer still creates the explicitly USD Session.
2. `TestPlanChangeExpiresPreviousSession`: terminal unpaid expiry permits a new choice; old open tab cannot pay; a race won by completion/processing prevents the replacement. Preserve attempt/history and isolation from another student's same email.
3. `TestWebhookAuthenticityAndReplay`: bad/expired signatures, wrong mode/API version and oversized payload cannot mutate; valid duplicates/out-of-order events converge on current state; DB failure is retryable; success-page forgery cannot fund anything.
4. `TestScheduledInvoiceCreationRecoversEveryBoundary`: crash/failure between invoice/item/arming stages resumes same resources, never finalizes an empty invoice or creates N+1 tuition obligations. Pending/failed payment is not funding; exactly zero/two/five later invoices exist.
5. `TestReconciliationPreservesUnpaidAndFinancialAdjustments`: partial settlement is retained, cash is deduplicated across allocations, credits/other settlement are not revenue, void/uncollectible/deleted state does not falsely forgive tuition, refunds/disputes become visible without a replacement charge. Include a fully settled contract receiving a later refund/dispute/fraud warning with **no webhook**, discovered by the daily audit.
6. `TestSuspensionAndReinstatement`: just before/exact 14-day boundary; on-time payment with late webhook does not penalize; late payment before the first recovered sweep still records the missed deadline; concurrent sweeps produce one episode; no fee before staff request; one fee per continuous episode; original debt plus paid fee cannot reactivate; full required settlement reactivates once; future invoices remain armed. Replay historical late invoices after reinstatement without a second fee; correct one false episode while another true deadline remains; cancel an erroneous fee at every absent/uncertain/draft/open/paid stage without resurrecting it. Existing unrelated inactive records remain inactive.

Extend current customer/management tests for contact/consent/expiry precedence, read-only GET/HEAD, ownership of invoice/portal actions and new POST auth/CSRF. Preserve `internal/payments/plans_test.go` original-day/month-end contract. Run from repository root with the local disposable test DB:

```fish
docker compose up --detach --wait --wait-timeout 90 db
# Only if this dedicated database does not already exist:
docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
go test -race -tags=integration ./internal/billing -run 'Test(CheckoutConcurrencyAndRecovery|PlanChangeExpiresPreviousSession|WebhookAuthenticityAndReplay|ScheduledInvoiceCreationRecoversEveryBoundary|ReconciliationPreservesUnpaidAndFinancialAdjustments|SuspensionAndReinstatement)$'
go test -race -tags=integration ./...
```

### Actual hosted payment and native invoice proof

Prerequisites: sandbox catalog/config above, private environment values, one local Air server using existing `.env` convention, test staff account, browser, and Stripe forwarding. Reuse an existing Air process and restart it after environment changes; never run two. In a separate terminal:

```fish
stripe listen --forward-to http://127.0.0.1:3000/webhooks/stripe
# Put this listener's signing secret in private STRIPE_WEBHOOK_SECRET, then restart existing Air.
go tool air
```

If CLI forwarding emits a different event API version, pin its request/event generation to `2026-09-30.endive` where supported; otherwise register a matching-version sandbox webhook on an authorized non-production HTTPS preview. Do not disable SDK version verification to get a green smoke.

Use a real browser to create a link through `/management/`, submit valid student data/consent, and complete Stripe-hosted Checkout with test card `4242 4242 4242 4242`, future expiry and test CVC. Inspect actual Stripe resources and DB, not just the landing page. Perform independent full/three/six enrollments:

- Full: one $600 invoice/payment; zero later invoices; total settled $600, tuition remaining $0.
- Three: initial $208.34; exactly two $208.33 draft invoices, `auto_advance=true` and timestamps matching stored Puerto Rico dates; remaining $416.66, overdue $0 before next date.
- Six: initial $108.35; exactly five $108.33 draft invoices; remaining $541.65; no recurring Stripe object.
- Repeat/replay payment initiation in two tabs, deliver a duplicate signed event, restart the application after first payment before all drafts are armed, and replay completion: same payment/contract/drafts, never another initial charge.
- Change canonical contact details and expire the source link in disposable local data: new customer routes return 404; later invoice events still update the same enrollment, and staff billing/portal actions still work.
- Exercise initial/renewal authentication with `4000 0027 6000 3184` (always authenticate). After a successful initial Visa payment, replace the default in the sandbox with `pm_card_chargeCustomerFail` (or enter `4000 0000 0000 0341` while setting up a card) to make later charges fail; do not use that failing card for the initial-success fixture. Recover through the actual restricted portal, then prove the next invoice uses the new Customer default. Use official [test cases](https://docs.stripe.com/testing); card numbers belong only in Stripe's hosted test UI, never server PAN handling. Decline/processing/authentication-required states never show as received cash.
- Refund a sandbox charge and trigger a documented test dispute/fraud case: history/review state changes, received/refunded cash is not double-counted, no fresh tuition charge appears.

For native scheduled collection use a throwaway Go smoke harness with the real SDK, app billing service/worker and an isolated test schema. First try a Stripe test clock customer with real plan amounts/dates, advance past each installment and an extra billing period, and wait for the clock-ready event/state. Assert exactly three/six tuition invoice payments and no additional charge. Test Jan 31→Feb 28→Mar 31, leap year, and a Puerto Rico evening payment crossing UTC midnight against stored/API finalization timestamps.

**Pre-decided clock fallback:** support for Checkout with clock customers and clock-driven one-off finalization is unverified in official docs. If either is unsupported, retain actual hosted Checkout proof on a normal sandbox Customer and verify its real monthly draft timestamps. Seed separate synthetic sandbox-only obligations in the isolated smoke schema with exact amounts and due times a few minutes apart; drive the unchanged real worker/Stripe scheduler to prove automatic collection and no extra object/charge after the last due time. This does **not** accelerate Smart Retries. Do not add production time-warp flags or replace `Plan.Schedule`; calendar tests plus real monthly timestamps prove dates.

Exercise suspension/reinstatement with a disposable failed-invoice fixture whose due date is backdated in the isolated DB; reconcile its real Stripe state before the sweep. Confirm inactive at the boundary, future invoices remain scheduled, staff creates only the separate $25 fee, fee alone cannot reactivate, and settling original overdue invoices plus fee does. Verify auth/CSRF and same-email isolation. Separately prove a **Stripe-executed card retry**, not a manual `Pay` call or just `next_payment_attempt`/`attempt_count` (hard declines can schedule without executing). Prefer a working test clock; otherwise leave the real-time soft-decline invoice pending through the actual retry window, which Stripe documents can take a day or more, change its saved card to Visa before the retry, and observe a new executed attempt and `invoice.paid`. Observe retry exhaustion on an independent failing fixture; open/uncollectible outcome retains unpaid tuition. Until these actual events occur, report retry behavior as unverified and block live readiness; a few-minute scheduler smoke cannot substitute.

Check desktop/mobile, keyboard focus, no-JavaScript checkout/recheck, existing modal editor isolation while live billing refreshes, and honest processing/error text. Apple Pay/Google Pay appear only on eligible devices; verify on a capable device or report that device-level smoke limit, never fabricate wallet proof. Sandbox email delivery is not live email proof: Stripe documents suppressed automatic test emails.

Before any merge/push to `master`, satisfy the repository's documented non-production Railway preview requirement (`README.md:29`) and existing authorization policy; this plan does not authorize live charging or a production push. Production activation also requires actual retry proof, confirmed tax treatment/account pricing, live catalog/config/keys/webhook, and replacement of the currently documented test-only four-character staff-password policy. Do not silently enable live mode or expand this payment task into an unrequested authentication redesign.

## Assumptions & contingencies

- Cards and Apple Pay/Google Pay only at installment launch; no ACH or BNPL extension. Payment form and card-update UI stay on Stripe. Installment dates/totals and agreement wording stay unchanged.
- Suspension changes this application's retained Student status; staff still handles any external classroom/LMS access. Adding an external platform integration would require its actual contract, not a pretend revocation call.
- Application does not import old Stripe history or infer obligations from existing local choices. One submitted enrollment is one financial owner even when emails match.
- If account OAuth/credentials are unavailable at execution, finish all reachable code and deterministic checks but do not call the integration complete: real sandbox authorization is the precise missing prerequisite. Never substitute mocks for the hosted/native billing proof. If an account setting/feature prevents the documented invoice path, stop the affected Stripe setup and report the exact prerequisite; do not silently change dates, collection mode or introduce an unbounded subscription.
- Automatic tax remains off until business tax treatment and applicable registrations are confirmed. Preserve quoted totals; any necessary tax change is a business decision, not an implicit SDK default.
