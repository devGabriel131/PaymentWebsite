# PaymentWebsite

Enrollment, agreement and tuition-payment web app for one company: staff manage links, students and settings; each customer uses a private per-person link.

## Purpose and status

- Staff sign in at `/management/` to create links, review students and billing, and change settings. Customers open `/customer/{token}`, submit the student's details, accept the agreement and pay through Stripe Checkout. PostgreSQL stores only a link's SHA-256 digest, so a lost URL is replaced, not recovered.
- All data is mock and the app is **not live-ready**. [AGENTS.md](AGENTS.md) records `Production ready: false`, the one-Air-server rule and the Railway deploy rules; losing rows in a migration is acceptable.
- Server-side dependency changes (`go.mod`/`go.sum`, including the phone-number library, the Go toolchain, build or deploy config) need a verified preview deployment in a non-production Railway environment before a merge or push to `master`.

## Stack

- Go 1.27.3 or newer (keep `GOTOOLCHAIN=auto`), `net/http` and `html/template`, HTMX 2.0.11 from jsDelivr plus small page scripts in `static/`. No frontend build step.
- PostgreSQL through GORM; `github.com/stripe/stripe-go/v87` v87.0.0; Argon2id and SCS (PostgreSQL store) for staff auth.
- `github.com/nyaruka/phonenumbers/v2` pinned to v2.0.15. Only its root package is used, for offline PR/US validation and E.164 formatting: no carrier, geocoding or timezone lookups.
- Railway hosts the shared deployment. Air is a project-local Go tool: run `go tool air`, not a global `air`.

## Local development

```fish
test -f .env; or cp .env.example .env
docker compose up --detach --wait --wait-timeout 90 db
go tool air
# Stripe webhooks, in another terminal: put the printed secret in STRIPE_WEBHOOK_SECRET, restart Air
stripe listen --forward-to http://127.0.0.1:3000/webhooks/stripe
```

[compose.yaml](compose.yaml) runs PostgreSQL 17 on `127.0.0.1:5432` with local-only credentials. The server listens on `127.0.0.1:3000`; create a staff account first. Environment variable names:

- `DATABASE_URL` is required (the only variable the `user` CLI requires). `SESSION_COOKIE_SECURE` defaults to `true`, accepts a boolean, and is `false` for local HTTP. `PORT` unset binds `127.0.0.1:3000`; set binds all interfaces on that port.
- Required, or startup fails: `STRIPE_API_KEY` (an `sk_`/`rk_` key whose `test` or `live` prefix matches the mode), `STRIPE_MODE` (exactly `sandbox` or `live`), `STRIPE_WEBHOOK_SECRET`, `PUBLIC_BASE_URL` (origin only: https, or http on localhost in sandbox), `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID`, `STRIPE_PORTAL_CONFIGURATION_ID`.

The app never loads `.env`; Air does (`env_files` in [.air.toml](.air.toml)) and shell variables win. Restart Air after changing the environment. Keep exactly one Air instance: it owns port 3000 and `tmp/main`. Air rebuilds on `.go`, `.html`, `.css` and `.js` changes but does not refresh the browser.

Startup: connect to PostgreSQL, `AutoMigrate(models())`, load the Stripe catalog (15 s limit), start the billing worker, listen. `models()` is `auth.User`, `auth.Session`, `settings.Settings`, `links.Link`, `students.Student` plus `billing.Models()` (`Contract`, `Obligation`, `CheckoutAttempt`, `Payment`, `Suspension`, `WebhookEvent`). The catalog is six Stripe price lookup keys (`asvab_reinstatement_usd` and each plan's `asvab_<plan>_initial_usd` / `_remaining_usd`); each must be an active one-time USD price of the expected amount in the configured mode. `main.go` owns one SQL pool (10 open, 10 idle) with `SkipDefaultTransaction`, so multi-statement units need explicit transactions.

On SIGINT or SIGTERM the server stops accepting connections and drains requests for up to 10 s; then the billing worker stops, auth session cleanup stops and the SQL pool closes, in that order. Air's forced rebuilds are not that guarantee.

Each open management tab holds one server-sent-event stream, so one of the browser's six HTTP/1.1 connections per host: keep fewer than six management tabs open locally.

Management and login pages load only `/static/styles.css`; customer pages load only `/static/customer.css`; both load Google Fonts. The customer Content-Security-Policy is `customerCSP` in [internal/customer/customer.go](internal/customer/customer.go).

## Staff accounts

- Accounts exist only through the CLI: `go run . user add|passwd|revoke|remove|list`; `go run . --help` has usage and the username and password rules. No registration, no email recovery. It needs `DATABASE_URL` where it runs, not a running server or Stripe configuration. On Railway use `./user.fish`.
- Passwords are Argon2id hashes, entered twice at a hidden terminal prompt, never as arguments, environment variables or pipes. The 4-character minimum is temporary: raise it before production.
- Sessions: SCS in PostgreSQL, fixed 14-day lifetime that activity does not extend. The `management_session` cookie is HttpOnly, SameSite=Lax, scoped to `/management/`, and Secure unless `SESSION_COOKIE_SECURE=false`.
- `passwd`, `revoke` and `remove` invalidate the account's sessions; each protected request re-checks the account and its auth version. Sign-out deletes only that browser's session.
- One process-wide login limiter allows a burst of 2, then one attempt every 12 s. Accepted tradeoff: someone who keeps it spent blocks new sign-ins; existing sessions keep working.
- Only login, logout and `/management/` routes load staff sessions; customer routes, static files, the favicon and `POST /webhooks/stripe` bypass them.

## Railway

- Project `amused-light`, service `PaymentWebsite`, built by Railpack from `devGabriel131/PaymentWebsite` branch `master`; every push to `master` deploys. The environment is named `production` but is test-only: use no real customer or payment data. Never copy Railway credentials into `.env` or `.env.example`.
- Service variables: `DATABASE_URL=${{Postgres.DATABASE_URL}}`, `SESSION_COOKIE_SECURE=true`, `PORT=8080`, and the Stripe variables above with `PUBLIC_BASE_URL` set to the public HTTPS origin. Register `POST /webhooks/stripe` on that origin as a Stripe webhook endpoint and set its signing secret in `STRIPE_WEBHOOK_SECRET`.
- Startup fails when PostgreSQL, the migration, the Stripe configuration or the Stripe catalog fails, so a missing or invalid Stripe setting blocks the deploy. The health check is `/management/`: signed-out requests get 200 with the login form.
- `./user.fish add|passwd|revoke|remove <username>` and `./user.fish list` run the deployed account CLI over Railway SSH. They need Fish, the Railway CLI, `railway login` and an SSH key registered with `railway ssh keys add`; `--help` works offline. `add` and `passwd` need stdin and stdout on a terminal.
- `user.fish` targets `production`; put `--environment <name>` before the command to change that. It runs the deployed binary, not local changes. `railway run` and `railway shell` run locally and cannot reach the private database host.

## Architecture

- `main.go`: configuration, SQL pool, migration, Stripe catalog, route wiring, billing worker, graceful shutdown, `user` CLI entry.
- `internal/auth`: staff accounts, CLI, login/logout, SCS sessions, `auth.Staff` (the verified username).
- `internal/billing`: Stripe config and catalog, Checkout, webhook intake, background worker, models, per-student summaries; `billingtest` builds a fake-Stripe service for other packages' tests.
- `internal/customer`: customer pages: contact form, agreement, plan choice, Checkout start and return.
- `internal/links`: link store: issuance, digest-only lookup, expiry, cancellation, drafts, enrollment, agreement.
- `internal/live`: in-process change feed and the SSE stream that refreshes open management pages.
- `internal/logging`: structured JSON logs, request correlation, slow-query summaries.
- `internal/management`: staff pages for links, students with billing, and settings.
- `internal/payments`: the three plans, integer-cent amounts, Puerto Rico schedules, formatting.
- `internal/settings`: the single settings row (link lifetime, program start date).
- `internal/students`: student store, contact-field validators, municipalities.
- `internal/testdb`: isolated PostgreSQL schemas for integration tests.
- `static/` (embedded CSS, scripts, images), `user.fish` (Railway SSH launcher), `compose.yaml` (local PostgreSQL).
- Dependencies are explicit: no globals or `init()` setup. `live.Feed.Changed` goes to the link, student and billing stores, each calling it after a committed write. After a staff edit of a student contact field, management calls `billing.TouchContractForStudent`, marking the Stripe-bound contract due and waking the worker.
- Customer routes and management share one link store; management also gets the student store. Enrollment (contact submission plus student creation) is one atomic SQL command in the link store, not a student-store transaction.
- `main.go` starts `billing.Service.Run` in one goroutine, cancels it on exit and waits for it. Folder separation is not an authorization boundary: `auth.Require` on the `/management/` mux is.

## Customer flow

| Route | Behavior |
| --- | --- |
| `GET /customer/{token}`, `GET /customer/{token}/forms` | Welcome and contact form; `HEAD` works on both |
| `POST /customer/{token}` | Final submit: validates, enrolls, redirects to the agreement |
| `POST /customer/{token}/draft/{field}` | Autosaves one field on blur; answers status only |
| `POST /customer/{token}/pin` | Reports whether a PIN is free; saves nothing |
| `GET`/`POST /customer/{token}/edit` | Corrects the Student's contact fields after submit |
| `GET`/`POST /customer/{token}/agreement` | Shows and accepts the terms |
| `GET`/`POST /customer/{token}/payments` | Plan choice; POST starts hosted Stripe Checkout and redirects to Stripe |
| `GET /payments/return` | Token-free Checkout outcome by `session_id`; HTMX polls while pending |
| `POST /webhooks/stripe` | Signed Stripe events; mounted in `main.go`, owned by billing |

- **Tokens:** 32 `crypto/rand` bytes, 43-char base64url. Only the SHA-256 digest is stored. Unknown, malformed, modified, canceled, and expired tokens return the same branded 404 (`HX-Refresh` for HTMX).
- **Expiry:** every operation checks `expires_at > now()` on the database clock, so expiry is a hard cutoff mid-form. Saved drafts stay.
- **First visit:** only `GET` of the welcome or form records it. `HEAD` and `POST` never do; GET-based link previewers can mark a link Opened.
- **Precedence:** unusable link, 404; already submitted, advance to the agreement (stale draft: 409); only then 400/413/422. Bodies cap at 4 KiB (`MaxBytesReader`) and each request's database work at 5 s.
- **Submit-and-enroll** is one SQL statement: a guarded `UPDATE links` feeds `INSERT INTO students`. It commits the snapshot, `submitted_at`, 168 h of continued access (`expires_at = now() + 168 h`), and one Student together. A failure rolls back and keeps the draft and initial expiry. Replays never re-enroll or move the deadline; cancel and submit are guarded updates, so one wins.
- **Link vs Student:** the Link keeps the submission snapshot; the Student is the canonical record. `/edit` writes only the Student through `billing.UpdateContact`, which locks link, contract, and attempts. It refuses once the contract is funded or an attempt is `processing`, `uncertain`, or `expiring`; blocked visits redirect to `/payments`. An unchanged form skips the write; a changed one marks the contract for Stripe customer sync.
- **Agreement:** acceptance stores database time and a snapshot of the program start date, atomic against the settings row (`FOR SHARE`). If staff changed the date after the page loaded, the POST returns 422 with the new date and consent unchecked. Replays keep the first values; with no configured date nothing can be accepted. The canonical text is `internal/customer/templates/agreement.html`. Acceptance stores no text version, so editing the template changes what past acceptances cover.
- **Plans** (`internal/payments/plans.go`): Pago Único $600.00; 3 monthly payments $625.00 ($208.34, $208.33, $208.33); 6 monthly payments $650.00 ($108.35, then five of $108.33). The page previews schedules from today's Puerto Rico date (UTC−4), clamping the day to month end.
- **Checkout:** `POST …/payments` calls `billing.StartCheckout` and redirects to Stripe. The plan can change before funding; once funded, a different plan returns 409. The return page only reads the persisted attempt and never funds anything; funding comes from verified Stripe state (see [Billing](#billing)).
- **Bearer tokens:** anyone with a usable URL can edit the unfinished fields, and after submit can correct contact details until payment starts. Reissue and identity checks are in [Open decisions before live mode](#open-decisions-before-live-mode).
- **Headers:** `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, and the customer CSP (same origin, jsDelivr for HTMX, Google Fonts, `form-action` to `checkout.stripe.com`, no framing). Redirects carry no contact data.
- **Messaging:** the app sends no email or SMS. Email and phone are syntax-checked, never verified.

Six contact fields come from one registry (`students.Field`, `internal/students/fields.go`). The same parsers serve submit, autosave, customer edit, and staff edit. Autosave lets an empty value clear a field; submit and edits require all six. The PIN and group are staff-only fields that only a Student holds (`students.StaffFields`): staff see and edit them, but they are never drafted in a Link, so `/draft/pin` and `/draft/group` are 404s and the customer edit form has neither.

| Field | Rule |
| --- | --- |
| Name | Letters, combining marks, and spaces only; two or more words on submit and edit (one-word drafts allowed); internal spaces collapse |
| Date of birth | `YYYY-MM-DD`, strictly before today in Puerto Rico, age 0–100 |
| Email | One bare ASCII mailbox, ≤254 bytes, dotted domain, domain lowercased. The confirmation is retyped, compared, never stored |
| Phone | Default region PR; digits, spaces, `().-`, leading `+`; valid PR or US numbers only; stored as E.164 |
| Municipality | Exact match: 78 municipios plus `Fuera de Puerto Rico`, 79 values |
| Gender | Exactly `Male` or `Female`; the customer form shows Masculino and Femenino |
| PIN | 4 to 6 ASCII digits, kept as text so leading zeros survive. Unique among students: a partial unique index (`idx_student_pin`, rows with an empty PIN excluded) decides at write time. The customer form checks on blur through `/pin`; a lost race at final submit rolls the link back unsubmitted and redraws the form with 422. Staff edits onto a taken PIN also give 422 |
| Group | Exactly `Básico` or `Regular`, enforced by a check constraint. Enrollment starts every student in `Básico` (the column default); staff promote or correct it from the student dialog |

## Management

| Route | Purpose |
| --- | --- |
| `GET /management/dashboard` | Business figures at a glance |
| `GET /management/`, `GET /management/trash` | Customer links; Trash |
| `POST /management/links`, `/management/links/{id}/cancel`, `/management/links/cancel` | Create; cancel one; cancel all unfinished |
| `GET`/`POST /management/settings`, `POST /management/program-start-date` | Link lifetime; agreement start date |
| `GET /management/students` (`?status=active` or `suspended` filters by `is_active`; any other value lists everyone), `/management/students/{id}`, `GET`/`POST …/{id}/fields/{field}` | List; detail dialog; per-field edit |
| `/management/students/{id}/billing` (GET), `…/billing/reconcile`, `…/reinstatement`, `…/portal` (POST), `…/invoices/{obligation}` (GET) | Billing fragment and staff billing actions |
| `GET /management/events` | Server-sent change feed |

- **Links:** create shows the URL once (only the digest is stored); a lost URL means a new link. Statuses: Not opened, Opened, In progress (any saved field), Finished. Only usable unfinished links cancel. Finished links leave the main list 24 h after submission and never enter Trash. Trash holds Canceled and Expired unfinished links, with drafts and audit kept; nothing is deleted.
- **Settings:** link lifetime 2–14 days, default 7, applies to new links only. The program start date feeds new agreements; accepted agreements keep their snapshot.
- **Dashboard:** read-only, refreshed live like the lists. Windows are fixed at 30 days (`dashboardWindow`): enrollment looks back, installments due looks ahead. Tuition figures count only funded contracts, because an abandoned Checkout leaves unpaid obligations nobody owes yet (`billing.Service.Overview`). Overdue, failed and in-review follow a student's billing rules, so totals add up to the student pages. Collected is cash received less refunds, before Stripe fees. Overdue-by-the-clock and the 30-day windows show only on the next load or refresh, like other time-based moves.
- **Students:** one record per completed submission, not a deduplicated person; it outlives its link. An edit changes one column under a row lock; a normalized no-op skips the write and the live event; invalid input gives 422. The Link snapshot never changes. A saved edit marks the student's Stripe-bound contract due and wakes the billing worker, which pushes name and email to the Stripe customer. Unlike customer edits, staff edits are not locked by funding or in-flight payments.
- **Cohort:** enrollment stores the month (`YYYY-MM`, check-constrained) of the agreement start date the customer accepted, so later changes to the settings date never move existing students. No edit reaches it, and promotion keeps it. The list and dialog show it as `November 2026`.
- **Privacy:** the link registry projection (`links.Entry`) exposes only name and phone, never email or municipality.
- **Audit:** creator, canceler, and settings editor come from the verified session (`auth.Staff`), never from form input; stores refuse these writes without one.
- **Billing actions:** reconcile re-reads existing Stripe state and never creates a charge; reinstatement requests the fee invoice; hosted invoice URLs are fetched on demand, never stored; the portal link is a payer credential, uncached. See [Billing](#billing).
- **HTMX:** link actions answer a notice plus `HX-Trigger: management-changed`; lists then re-fetch rows only, so a stale render cannot overwrite a newer one.
- **Live updates:** events carry no record data (`data: change`); one on connect, a heartbeat comment every 30 s (Railway closes idle streams after five minutes), and a 5 s write deadline per event that replaces the server's 10 s `WriteTimeout`. Stores and billing call `live.Feed.Changed` after committed writes. The in-process fan-out fits one Railway replica; move to PostgreSQL `LISTEN/NOTIFY` before running more. Time-based moves (expiry, 24 h archive) show only on the next load or refresh.
- **Limits:** link and student lists are unpaginated for two staff and mock data. `http.CrossOriginProtection` wraps every route in `main.go` and rejects cross-origin browser writes; Stripe's webhook sends no browser origin headers and passes.

## Billing

The first payment is hosted Checkout. Each later installment is a one-off Stripe invoice armed with `automatically_finalizes_at` at its stored due date (Puerto Rico midnight; Stripe promises no exact second). There is no Stripe subscription.

- One contract per student. A partial unique index allows one active Checkout attempt per contract. Repeat starts resume it; a timeout or 5xx leaves it `uncertain` for the worker, never a second Checkout.
- Funding comes only from verified Stripe state read by the worker. `/payments/return` shows the stored attempt and never funds. The app never charges a card itself; Stripe retries failed invoices. An invoice with tax, a discount, a balance debit, another currency or an unexpected line is held (`auto_advance` off) for staff review.
- Installment plans need a `card` first payment method; otherwise the contract is flagged `unsupported_payment_method` and no later invoice is armed. Checkout therefore sends installment plans `allowed_payment_method_types=[card]` and no payment-method configuration (Stripe rejects both together); Apple Pay, Google Pay and Link ride on card. The single payment uses the payment-method configuration. Every invoice the app creates is limited to `card`, so Klarna never appears on later installments or the reinstatement fee.
- A tuition installment unpaid 14 days after its due date suspends the student (`is_active=false`); later invoices stay armed. Staff request the $25 reinstatement fee, a separate invoice paid on Stripe's hosted page. Reactivation is automatic once the fee and all due tuition are settled and no review is open. Suspension changes only `is_active`; staff end classroom access outside this app.
- Overdue tuition never goes onto a new Checkout or fee invoice. The original invoices stay collectible; the payer settles them on Stripe's hosted invoice pages or a staff-issued portal link.
- `POST /webhooks/stripe` sits outside staff auth and reads at most 1 MiB. `webhook.ConstructEvent` checks the signature, its tolerance and that the event's API version is on the SDK's release train (`2026-09-30.endive`). A bad signature, JSON, version or mode returns 400; an oversized body 413. An allowlisted event is stored by ID, without payload, before the 200; a DB error returns 500 so Stripe retries. Other types get 200 and no work.
- The billing worker runs every 5 seconds and on each stored event: up to 25 events, then up to 10 due contracts. It re-reads current Stripe objects and never applies an event body, so replays and reordering converge. A contract sync covers Checkout attempts, invoice arming, payments, refunds, disputes, the 14-day sweep and reactivation; funded contracts re-sync at least daily. Claims use `FOR UPDATE SKIP LOCKED` and 60-second leases, so replicas never work the same event or contract.

Startup fails (`startup_failed`) without valid Stripe configuration or a loadable catalog; `user` and `--help` need neither. Set up Stripe once per mode:

- Environment: `STRIPE_API_KEY` (restricted; prefix must match mode), `STRIPE_MODE` (`sandbox` or `live`), `STRIPE_WEBHOOK_SECRET` (the registered endpoint's, not `stripe listen`'s), `PUBLIC_BASE_URL` (origin only; https except localhost in sandbox), `STRIPE_PAYMENT_METHOD_CONFIGURATION_ID`, `STRIPE_PORTAL_CONFIGURATION_ID`.
- Prices: one-time, `per_unit`, USD, active, in the configured mode, found by lookup key and amounts from `internal/payments/plans.go`: `asvab_full_initial_usd`, `asvab_three_initial_usd`, `asvab_three_remaining_usd`, `asvab_six_initial_usd`, `asvab_six_remaining_usd`, `asvab_reinstatement_usd` (2500 cents). Any other state stops startup; there is no fallback price.
- Webhook endpoint at `<PUBLIC_BASE_URL>/webhooks/stripe`, created with API version `2026-09-30.endive` and subscribed to the types in `webhookEvents` (`internal/billing/webhook.go`).
- Payment-method configuration (used by the single payment only): Card, Apple Pay, Google Pay, Link and Klarna on, everything else off. Customer portal configuration: invoice history and card update only.
- Adaptive Pricing off (also for Hosted Invoice Pages), automatic tax off. Smart Retries on for one-off invoices with a 2-week window, matching the 14-day suspension rule; the window is dashboard-only (Settings > Billing > Invoices > Advanced invoicing features), so the app cannot check it.
- Customer emails (dashboard-only): in Settings > Emails, enable "Successful payments" for receipts. In Settings > Billing > Subscriptions and emails (Automatic collection), enable "Send emails when card payments fail" and "Send a Stripe-hosted link for cardholders to authenticate when required" (3D Secure).
- Stripe pages, emails and PDFs use locale `es-419` (`stripeLocale` in `internal/billing/stripe.go`): Spanish with US-style numbers, printed `USD 600.00`. `es` prints Spain-style `600,00 US$`; only `en` prints a bare `$600.00`, in English.

## Logging

- Output is newline-delimited JSON on stdout: lowercase `level`, a stable event name in `message`. Root metadata comes only from the Railway allowlist, with fallbacks `PaymentWebsite`, `local`, `unknown`. CLI diagnostics go to stderr.
- `X-Request-ID` reuses one valid `X-Railway-Request-Id` (1-128 of `A-Za-z0-9._-`), else a random ID. A client `X-Request-ID` is ignored. IDs aid diagnosis; they are not credentials.
- A failed request logs at most one `request_failed`, labelled by the registered route pattern, never the URL. Success and expected 4xx are silent. Errors expose only `error_kind`, `error_type` and SQLSTATE; SQL, bind values, cookies, URLs, query strings and submitted values are never logged.
- GORM queries slower than 200 ms are counted per route pattern or background name and flushed every 30 seconds as `database_slow_summary`, one line per label, to stay under Railway's 500 lines per second limit. SCS session SQL bypasses GORM, so it is not timed.
- Lifecycle events are `server_listening`, `shutdown_requested`, then one `shutdown_completed` or `shutdown_failed`. Billing events carry only opaque references: `billing_reconcile_failed`, `billing_review_required`, `stripe_webhook_rejected`. Use Railway's HTTP logs for traffic and latency.

## Verification

Integration tests need a dedicated local database. Create it once, then run the checks:

```fish
docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
go build ./...
go vet -tags=integration ./...
go test -count=1 ./...  # unit tests, no database
go test -tags=integration -count=1 ./...
go test -race -tags=integration ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/tools/cmd/deadcode@latest -test -tags=integration ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Each integration test gets its own Postgres schema (`internal/testdb`). `internal/billing/billingtest` fakes Stripe; no test calls Stripe. Tests do not prove randomness statistically. No test proves these browser checks:

- Native dialog focus after an out-of-band row replacement; clipboard copy toast and failure recovery.
- Live updates across tabs, and reconnect after a restart with URL and focus kept; the 24-hour archive cutoff on the next load.
- Layouts at 320x568, 390x844 and 1440x900 with no horizontal overflow.
- JavaScript-disabled flows through submit, agreement and payments.
- Hosted Checkout round trip with Stripe sandbox cards, and wallet display on a real device.

## Open decisions before live mode

- Link reissue: a new link never revokes older ones, and links are not tied to a person. Decide whether re-sending should revoke the earlier link.
- Customer verification and identity: links are bearer credentials and valid contact syntax is not identity; each completed submission creates its own Student and contract, with no dedup by email or phone.
- Smart Retries: no proof yet that Stripe itself ran a retry on a one-off invoice (not a manual pay, not just a scheduled next attempt), or of retry exhaustion. Treat retry behavior as unverified.
- Money: tax treatment and registrations (automatic tax stays off), Adaptive Pricing on the live account, and the actual Stripe fees.
- Go-live: live catalog, restricted key, configurations and webhook endpoint; Apple Pay and Google Pay on a real device; live email delivery (sandbox suppresses automatic emails; verify dashboard toggles for successful payment receipts, failed payment alerts and 3DS links).
- Staff passwords: raise the 4-character minimum in `internal/auth/cli.go`.
