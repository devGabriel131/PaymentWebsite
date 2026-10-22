# PaymentWebsite

A company payment application with separate management and customer interfaces, one shared Go backend, and PostgreSQL persistence through GORM. The target workflow uses customer-specific invoice links; shared customer links are not part of the design.

## Current status

The runnable slice implements CLI-managed staff access, persistent sign-in, hash-only customer links with configurable expiry, clipboard copying, unfinished-link cancellation, student contact autosave, a retained Students archive, agreement acceptance, and local payment-plan selection:

- `/` redirects to `/management/`. Signed-out staff see a login form; signed-in staff see a dark workspace with **Customer links**, **Students**, and **Settings** at the top of the desktop sidebar, **Trash** anchored at the bottom-left even while scrolling, their username, and **Sign out**. The shared link registry is horizontally centered in the viewport, with status badges and an empty state. At widths of 960px and below, all navigation moves above the content and wraps normally.
- **Settings** (`/management/settings`) has a slider for initial access to new, unfinished customer forms: 2–14 days in one-day steps, 7 by default. Saving records who changed it. Existing unfinished links keep their issued expiry when the slider changes. Completing the contact form replaces that deadline with exactly seven days of continued enrollment access.
- Staff accounts are created through the CLI, not a registration page. Passwords use Argon2id; SCS stores sessions in PostgreSQL for a fixed 14 days. Password changes, revocation, and account removal invalidate existing sessions.
- **Create & copy link** copies a new URL and shows it on the new row. The URL exists only in that creating response and the page that received it: PostgreSQL stores just its digest, so it can't be recovered later. Live updates keep it on its row until the page reloads. A lost URL is replaced by creating a new link.
- Every staff account sees the same saved links after reloads and server restarts, and open management pages update live: any committed customer or staff change refreshes **Customer links**, **Trash**, and **Students** without a reload (see [Live updates](#live-updates)). **Customer links** shows **Not opened**, **Opened** (first visit recorded), **In progress** (any saved contact field, not yet submitted), and **Finished** (submission committed with its student record). Finished links leave this page at 24 hours after completion, on the next load or live refresh; they never enter Trash. **Trash** (`/management/trash`) shows only **Canceled** and **Expired** unfinished links, retaining saved drafts and audit history without creation or cancellation controls. Both pages expose only **name and phone** as personal information, with phone rendered as plain text; email and municipality are absent from their projection and HTML. Status, creation/completion/access-expiry times in the viewer's time zone, audit, one-time URL/copy, and applicable cancellation controls remain. There is no timer, polling, or deletion job.
- **Students** (`/management/students`) lists every completed contact submission, newest first, with editable canonical **Name**, **Email**, **Phone**, **Municipality**, and **Submitted** time. Each completion creates one record tied to its source link, not a deduplicated person: submitting the same details through two links creates two records. Students remains available after its source link expires or leaves the main page. Link and student history are deliberately unpaginated for two staff and small mock data; add pagination before sustained growth.
- Open a student through its name link or row to see centered native-dialog details; the pencil edits one field, **Save** persists it and updates the table row, and **Cancel** reloads the saved field. Municipality uses a native select. Each edit changes only that canonical Student column, never another student, another field, the source link's four-field submission snapshot, or the customer's locked state. Real links and ordinary forms provide full detail/edit pages without JavaScript.
- `POST /management/links` persists the random token's digest, then returns the new URL, its copy button, and a notice for HTMX, or the filtered main-page list for ordinary form submissions.
- `POST /management/links/{id}/cancel` revokes one usable, unfinished link by its management ID; `POST /management/links/cancel` revokes every usable, unfinished link matched when its database update runs, so links created afterward, from any tab or account, stay active. Revocation moves the row to **Trash** as **Canceled**, keeping its draft and audit history; there is no deletion UI. Finished links cannot be canceled by either endpoint, even from a stale page or direct request. Expired unfinished links remain **Expired**.
- HTMX creates and cancels answer only with their notice (plus a created URL) and `HX-Trigger: management-changed`; the page then re-fetches its list. Refreshes are the only writers of rows, so an older render never overwrites a newer action. Each row keeps hidden `hx-preserve` slots for its one-time URL and **Copy link**, so a URL stays on its row through every refresh until the page reloads or the row leaves the list. An unknown or malformed ID answers HTTP 204 with no notice, and a failed cancellation answers HTTP 500 without a notice. Ordinary form submissions, reloads, and navigation render the relevant filtered list, dropping any one-time URLs still on screen.
- While a create or cancel request is in flight, the page disables its buttons, so the normal UI sends one change at a time; a live refresh landing meanwhile renders fresh, enabled rows. This is not request serialization: other tabs, other staff, and direct requests can still interleave, and each is applied on its own.
- Each row is an audit trail: "created … by gabriel · canceled by worker". The username comes from the verified server-side session, never from form input, and the store refuses creation or cancellation without one. **Cancel all unfinished links** records its canceler only on usable, unfinished links it cancels.
- `/customer/{token}` opens the Spanish welcome; **Próximo** opens `/forms`. **Enviar información** atomically saves the four normalized contact fields, submission timestamp, seven-day deadline, and one student, then advances to `/agreement`. Accepted terms unlock `/payments`, where the customer can save or change one of three payment choices. All steps work without JavaScript; contact autosave uses HTMX. Unknown, modified, canceled, and expired tokens return the same branded 404. Contact submission is locked against replacement or duplicate enrollment; stale keyed drafts return 409.
- **Fecha de comienzo** above **Link registry** saves the program date through `POST /management/program-start-date`. The agreement cannot be accepted until staff configures a valid date. Accepted agreements keep their original date when staff changes the setting.
- Agreement acceptance and plan selection persist locally. They do not create a checkout, charge money, start recurring billing, or confirm payment. Stripe integration remains explicitly deferred.

Links are not invoice-bound or matched to a unique person yet. Students holds canonical contact details for each completed submission, not a unique customer identity or verified mailbox/phone. Invoices and actual payment processing remain outside this slice. Sign-in expiry does not expire customer links.

**Test/development only:** management is authenticated, but invoice and payment workflows are not implemented. Use disposable data, never real customer/payment data.

The existing server-side phone dependency requires a verified non-production Railway preview before any future merge or push to `master`.

## Stack and development

- Go 1.27.3 or newer, using `net/http` and `html/template`. Keep `GOTOOLCHAIN=auto` so `go` selects the version required by `go.mod`.
- HTMX 2.0.11 for server-rendered interactions; mobile-first pages and small page-specific JavaScript.
- PostgreSQL for persistence, accessed through GORM and its PostgreSQL driver.
- `github.com/nyaruka/phonenumbers/v2` pinned to **v2.0.15** for offline PR/US number validation and E.164 formatting; only its root package is used, without carrier/geocoding/timezone lookups. Name and email rules use the Go standard library.
- Railway hosts the shared test deployment; see [Railway test deployment](#railway-test-deployment).
- Air is installed as a project-local Go tool.

Start local development from the project root. The copy step preserves an existing `.env`:

```fish
test -f .env; or cp .env.example .env
docker compose up --detach --wait --wait-timeout 90 db
go tool air
```

Keep `SESSION_COOKIE_SECURE=false` in private `.env` for local HTTP. Air loads it and `DATABASE_URL` from `.env`; shell variables take precedence. Local `.env` files are ignored by Git.

The database credentials in `compose.yaml` and `.env.example` are for local development only. PostgreSQL is bound to `127.0.0.1:5432`; its named volume preserves issued links between container restarts. Start the Compose service before Air if it is not already running.

The Go binary requires `DATABASE_URL` in both server and CLI modes; the Railway launcher uses the deployed environment, not local credentials. `SESSION_COOKIE_SECURE` defaults to true. Startup runs one `AutoMigrate(&auth.User{}, &auth.Session{}, &settings.Settings{}, &links.Link{}, &students.Student{})`. Link and Student embed the same non-null text `students.Details`; empty defaults support unfinished drafts and existing mock rows, while final submissions and canonical edits require validation. Additive agreement fields live on Link; Settings holds the program date; Student adds `is_active NOT NULL DEFAULT true`. No compatibility adapters, migration framework, backfill, or database reset is needed.

`main.go` owns the shared SQL pool and GORM configuration: maximum open and idle connections are both 10, and `SkipDefaultTransaction: true` avoids GORM's per-write transaction wrapper. PostgreSQL statement atomicity remains intact, including atomic completion/enrollment in one SQL command. Genuine multi-statement units still require explicit transactions. Writes are synchronous and acknowledged only after database success; there is no application cache, write queue, or background draft worker.

Restart an existing Air process after changing its environment so it reloads `.env`. Keep one instance running. Without Air, export the required variables before `go run .`; the application never loads `.env` itself or silently falls back to local credentials.

Use one server or Air instance at a time: they share port 3000, and Air instances also share `tmp/main`. Keep the Air dev server running and reuse it (see `AGENTS.md`).

Open <http://127.0.0.1:3000/management/> and sign in using a CLI-created account. **Create & copy link** copies the same-origin URL and shows it on the new row; **Copy link** recopies it until the page reloads, even as live updates refresh the list. Each successful copy shows **Link has been copied.** in a toast that rises into the bottom-right corner, then fades out; with reduced motion it only fades. Clipboard access requires JavaScript and HTTPS or localhost. Creation feedback waits until the new URL is visible in its registry row. If clipboard access is blocked, the status offers manual copying only then; failed creation leaves the clipboard unchanged.

Air watches Go, HTML, CSS, and JavaScript files. Its generated defaults do not automatically refresh the browser; refresh manually. Use `go tool air`, not a separately installed global `air` command. The existing `.air.toml` was generated with `go tool air init`.

On interrupt or SIGTERM, the server stops accepting new connections and allows in-flight handlers up to ten seconds to finish. Open live-update streams end as soon as shutdown starts, so they never hold the drain. A failed graceful shutdown force-closes remaining connections; auth session cleanup stops before the SQL pool closes. Active request contexts are not canceled just because shutdown starts. Air's forceful development rebuilds are not this production drain guarantee.

Both interfaces load the embedded `/static/styles.css`. Management keeps its scoped dark palette; customers keep the light brand palette. The welcome is centered. Contact forms use a compact logo, one 28rem column, left-aligned labels, and native controls with at least 16px text. Agreement pages use a readable 44rem document; payment options compare in three columns on desktop and stack on mobile. All terms and dates remain in normal page flow, with safe-area padding, visible focus, and touch-sized actions. The branded invalid-link page uses `.customer-not-found`. Air rebuilds embedded asset edits; deployment needs no separate asset directory.

Every full page declares the same favicon: `static/favicon.ico`, using the original logo's bulb/brain emblem so it stays recognizable at tab size. The ICO contains 16×16, 32×32, and 48×48 images for standard and high-density displays; the full logo is unchanged. Restart Air after regenerating the ICO so the embedded asset is rebuilt.

Typography currently uses **JetBrains Mono** across both interfaces, including form controls and management branding. Each full-page template loads variable weights 400–700 from Google Fonts alongside the shared stylesheet, rather than waiting for a CSS import, with `font-display: swap`; unavailable font requests fall back to the system monospace font.

No frontend build step is required. Air's generated `tmp/` directory is ignored by Git.

## Staff accounts and sessions

Create the two staff accounts explicitly. For hosted accounts, use the [Railway account launcher](#railway-account-commands). For the default local database:

```fish
set -gx DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite?sslmode=disable'
go run . user add gabriel
go run . user add worker
go run . user list
```

Replace example usernames as needed. Usernames are case-insensitive, normalized to lowercase, and accept 1–64 ASCII letters, digits, `.`, `_`, or `-`. Passwords require at least 4 Unicode characters and at most 1024 bytes, entered twice at a hidden terminal prompt. Password arguments, environment variables, and piped plaintext are not accepted. The Go account commands need `DATABASE_URL` where they execute, but not a running web server.

The four-character minimum is temporary for mock-data development; raise it before production.

`user passwd <username>` changes a password and invalidates prior sessions. `user revoke <username>` invalidates sessions without changing the password; `user remove <username>` removes the account, not its links. Recreating a removed username cannot revive old cookies. Duplicate creation never overwrites an existing password.

Both staff have the same access to the link registry; the separate accounts exist so each action records who did it. Sessions contain account identity/version, not link history. Persistent HttpOnly, SameSite=Lax cookies remember sign-in across tabs and browser restarts; ordinary visits do not extend the 14-day deadline. Production cookies are Secure even behind Railway's TLS proxy. Login attempts from every client share a per-process limit of two initial attempts, then one every 12 seconds. This bounds password guessing and Argon2 memory use; the accepted tradeoff is that anyone who keeps the limit spent blocks new staff sign-ins until they stop, while existing sessions keep working. There is no registration or email recovery.

`auth.New(db, secureCookies, log)` owns SCS construction, cookie settings, and expired-session cleanup. The `management_session` cookie is scoped to `/management/`; SCS wraps login/logout and protected management handlers only. The root redirect, customer routes, static files, and favicon bypass staff-session loading, even when sent a forged staff cookie. Protected requests still verify the current account version against PostgreSQL. `Manager.Close()` cancels cleanup and waits for its worker, leaving the shared SQL pool open.

Postgresstore still persists sessions synchronously, with its own cleanup worker disabled. Auth runs one expiry sweep every five minutes, without an immediate startup sweep, using the same `expiry < CURRENT_TIMESTAMP` predicate and a five-second database timeout. Failed sweeps emit safe `session_cleanup_failed` events; stopping the worker is quiet.

**Sign out** posts to `POST /management/logout`, which deletes that browser's session row and expires its cookie; the account's other browsers stay signed in. To end every session for an account, use `user revoke`.

## Live updates

Open **Customer links**, **Trash**, and **Students** pages stay current without polling. Each holds a server-sent event stream from `GET /management/events`, behind staff sign-in, through the browser's native `EventSource`; the server side is standard-library `net/http`, with no htmx extension or added package. `static/management-live.js` turns every message into an htmx `management-changed` event, and each page's list re-fetches itself with an ordinary authorized htmx GET of its own URL, which answers with only rows or records. Messages carry no data, so a stream never reveals what changed.

- The link and student stores notify the `internal/live` feed after committed changes: creation, first visit, autosave, contact submission, agreement acceptance, plan changes, cancellation, and student edits. Unchanged replays stay silent. A burst of writes leaves each page one pending event.
- A stream sends one event on connect, so a page catches up on anything it missed while loading or reconnecting, and a comment heartbeat every 30 seconds keeps proxies from dropping it; Railway closes streams idle for five minutes and caps requests at 15. `EventSource` reconnects dropped streams itself. If a reconnect fails outright, the page refreshes once, which sends signed-out staff to sign in, and listens again five seconds later.
- Streams clear the server's 10-second `WriteTimeout` and end when graceful shutdown starts. Air's forced restarts cut them instead; pages reconnect either way.
- A created link's URL and copy button wait in a hidden `#link-tokens` holder, placed before the list, until the refresh renders the new row; its hidden placeholders, keyed by link ID, pull them in through `hx-preserve` and keep them through later refreshes. Pending creation feedback survives failed/background refreshes until a visible row consumes it; a newer staff action discards it. Cancel buttons and student name links keep stable IDs, so htmx restores keyboard focus after a refresh.
- Time-based moves still wait for the next load or live refresh: an unfinished link reaching its expiry or a finished link reaching its 24-hour archive has no write to announce.
- The feed fans out in process, which fits the single Railway replica; move it to PostgreSQL `LISTEN/NOTIFY` before running more than one. Over local HTTP/1.1, each open management tab holds one of the browser's six connections to `127.0.0.1:3000`, so keep fewer than six such tabs open.

## Operational logging

- Server events are newline-delimited JSON on stdout: lowercase `level`, a stable event name in `message`, and numeric millisecond durations. Root metadata uses only the Railway service/environment/commit/deployment/replica allowlist; local fallbacks are `PaymentWebsite`, `local`, and `unknown`.
- One outer request boundary returns `X-Request-ID`. It reuses a single `X-Railway-Request-Id` value only when it contains 1–128 ASCII letters, digits, `.`, `_`, or `-`; otherwise it generates a cryptographic ID. Client `X-Request-ID` is not a fallback. IDs aid diagnosis, not authentication.
- A reported failure or HTTP 5xx emits at most one `request_failed` after the complete handler/session stack returns. It includes method, token-free registered route, bounded operation, stage, actual committed status, and elapsed time. Successful and expected 4xx requests emit nothing. Request cancellation and render-stage broken connections are Warn; other unexpected failures are Error.
- Errors expose only `error_kind`, concrete `error_type`, and valid SQLSTATE when available. SQL, bind values, database messages, URLs, query strings, cookies, credentials, and submitted values never enter operational events. The stdlib HTTP diagnostic adapter discards raw panic values, addresses, and stacks without changing panic handling.
- Successful GORM queries strictly slower than 200 ms accumulate count and worst duration per fixed operation. Every 30 seconds, nonempty buckets emit `database_slow_summary` at Warn: at most 22 lines per scheduled flush, plus startup/final partial flushes. Empty flushes are silent; SQL callbacks are never evaluated. Failures remain owned by their request, setup, CLI, or cleanup boundary rather than waiting for a summary.
- Direct SCS persistence SQL remains outside GORM timing summaries. An isolated slow warning may wait one interval; SIGKILL can lose unflushed in-memory counts. There is no logging queue, exporter, or additional access-log stream.
- `server_listening` follows a successful bind. Shutdown emits `shutdown_requested`, then exactly one `shutdown_completed` or `shutdown_failed`, preserving the ten-second drain. CLI diagnostic JSON uses stderr; human help, account output, prompts, and command errors retain their operator-facing format.
- Use Railway's native HTTP logs for traffic, latency, and error-rate investigation. Scope application failures to the app service's structured Error events; PostgreSQL checkpoint LOG output belongs to the database service. Railway severity parsing and native correlation require a separately approved deployment to verify live.


## Railway test deployment

The `PaymentWebsite` service in the existing `amused-light` project deploys `devGabriel131/PaymentWebsite`, branch `master`, using Railpack. Railway names this environment `production`; the application is still for testing only.

Service configuration:

- `DATABASE_URL=${{Postgres.DATABASE_URL}}` references the project's PostgreSQL service over Railway's private network. Local `.env` credentials are not uploaded or reused.
- `SESSION_COOKIE_SECURE=true` keeps management cookies HTTPS-only.
- `PORT=8080` makes the application listen on all interfaces for Railway routing. Without `PORT`, local startup is bound to `127.0.0.1:3000`.
- The health check remains `/management/`: signed-out requests receive HTTP 200 with only the login form. PostgreSQL connection and migrations must succeed before HTTP starts.
- Railpack uses the Go version required by `go.mod`; the deployed process does not run Air. Pushes to `master` trigger automatic deployments.

For a manual deployment from this directory:

```fish
railway up --project a0207263-4a1b-4174-b693-5830fd46e486 --environment production --service PaymentWebsite --ci -m "Update PaymentWebsite"
```

The public HTTPS domain serves both management and customer routes. PostgreSQL traffic stays private. Do not copy Railway database credentials into `.env.example` or commit a local `.env`.

### Railway account commands

Manage hosted accounts from your computer with `./user.fish`. The launcher runs the existing account CLI inside the deployed app container over Railway SSH, where the private PostgreSQL URL works:

```fish
./user.fish add gabriel
./user.fish add worker
./user.fish list
```

Use `./user.fish passwd <username>`, `./user.fish revoke <username>`, and `./user.fish remove <username>` for the other account operations. `./user.fish --help` works without connecting to Railway.

Prerequisites: Fish, the Railway CLI, authentication through `railway login`, and a registered SSH key through `railway ssh keys add`. If you have no SSH key, generate one with `ssh-keygen -t ed25519` first. The launcher needs neither a local Go toolchain nor `DATABASE_URL` or `.env`.

The target is explicitly `amused-light` / `PaymentWebsite`, defaulting to `production` independently of the CLI's linked context. To select another existing environment in that project, put `--environment <name>` before the account command.

Real staff enter their own passwords at the hidden prompts. Both stdin and stdout must be terminals for `add` and `passwd`; do not pipe or redirect those commands. Railway output and exit status pass through unchanged.

Railpack currently builds `/app/out`; the launcher runs that deployed binary, not local working-tree changes. `railway run` and `railway shell` execute locally and do not make Railway's private database hostname reachable; do not expose PostgreSQL publicly or copy production credentials into local `.env` for this CLI.

## Project structure

```text
.
├── main.go
├── user.fish
├── static/
│   ├── customer.js
│   ├── favicon.ico
│   ├── logo.png
│   ├── management.js
│   ├── management-live.js
│   ├── management-time.js
│   ├── students.js
│   └── styles.css
├── internal/
│   ├── auth/
│   │   ├── auth.go
│   │   ├── cli.go
│   │   ├── http.go
│   │   ├── http_test.go
│   │   └── templates/login.html
│   ├── logging/
│   │   ├── logging.go
│   │   ├── logging_test.go
│   │   ├── http.go
│   │   ├── http_test.go
│   │   ├── http_integration_test.go
│   │   ├── database.go
│   │   └── database_test.go
│   ├── live/
│   │   ├── live.go
│   │   └── live_test.go
│   ├── management/
│   │   ├── management.go
│   │   ├── management_test.go
│   │   ├── students.go
│   │   ├── students_test.go
│   │   └── templates/
│   │       ├── index.html
│   │       ├── settings.html
│   │       └── students.html
│   ├── customer/
│   │   ├── customer.go
│   │   ├── agreement.go
│   │   ├── agreement_test.go
│   │   ├── customer_test.go
│   │   └── templates/
│   │       ├── index.html
│   │       ├── agreement.html
│   │       ├── payments.html
│   │       └── not-found.html
│   ├── payments/
│   │   ├── plans.go
│   │   └── plans_test.go
│   ├── links/
│   │   ├── links.go
│   │   └── links_test.go
│   ├── settings/
│   │   ├── settings.go
│   │   └── settings_test.go
│   ├── students/
│   │   ├── students.go
│   │   ├── students_test.go
│   │   ├── fields.go
│   │   ├── fields_test.go
│   │   └── municipalities.go
│   └── testdb/testdb.go
├── compose.yaml
├── go.mod
├── go.sum
├── .air.toml
├── .env.example
├── .gitignore
└── README.md
```

- `main.go`: configuration, one current-model `AutoMigrate`, shared bounded SQL pool, live-update feed wiring, route wiring, HTTP timeouts, native cross-origin request protection, and signal-driven graceful shutdown.
- `user.fish`: local Railway SSH launcher for deployed staff account commands, preserving interactive password prompts without local database credentials.
- `internal/auth/`: staff credentials/CLI, embedded login form, sign-out, account-version authorization exposing the verified username (`auth.Staff`), and ownership of SCS setup, scoped middleware, and session cleanup.
- `internal/logging/`: the shared operational logging boundary for safe structured events, request correlation/outcomes, bounded GORM summaries, and stdlib HTTP diagnostics.
- `internal/live/`: the in-process change feed and the server-sent event stream that tells open management pages to refresh.
- `internal/management/`: shared link registry, settings, and authenticated Students routes/templates, including the rows-only fragments live refreshes fetch; `students.go` builds field views and handles detail/edit fragments, while the single `students.html` template owns list/detail pages and shared field/table-row rendering. `main.go` injects `auth.Staff` for link/settings audit attribution.
- `internal/customer/`: welcome/contact handlers, isolated autosave, transient confirmation, and private advancement redirects. `agreement.go` owns consent and local plan-choice routes; shared embedded templates render all customer pages.
- `internal/links/`: issuance, digest-only access, expiry, unfinished-only revocation, isolated drafts, atomic contact submission/enrollment, immutable agreement snapshots, and gated local plan changes. Management `Entry` projects only name and phone as personal information.
- `internal/settings/`: the single settings row, bounded link lifetime, validated program date, and staff audit. Independent column upserts preserve other settings.
- `internal/payments/`: the three read-only plan definitions, integer-cent amounts, Puerto Rico monthly schedules, and Spanish date/currency formatting. No provider integration.
- `internal/students/`: retained model/store, shared contact details and validators, municipality options, and the active flag. Reads retain inactive students and expired-link records; contact edits never rewrite Link snapshots.
- `static/customer.js`: contact-form-only email validity, draft accessibility, final-422 focus, detached-draft suppression, and network feedback. Welcome, agreement, and plan-selection pages load no scripts.
- `static/styles.css`: shared interface styles and scoped management/customer layout, embedded and served by the Go application.
- `static/management.js`: link-page-only native clipboard copying and its bottom-right confirmation toast, handoff of a created URL until the live list shows its row, and request-failure/clipboard sequencing that ignores background refreshes; creation and cancellation use declarative HTMX forms.
- `static/management-live.js`: shared `EventSource` bridge that turns change messages into htmx refreshes of each page's list.
- `static/management-time.js`: shared local-time formatting for link/Trash rows and Students/detail pages, including HTMX-swapped content.
- `static/students.js`: student row activation, native dialog and editor focus, and scoped HTMX validation/network-failure handling; fetching, saving, and canceling remain declarative HTMX interactions.
- `compose.yaml`: local PostgreSQL service and persistent development volume.
- `.env.example`: local database connection template. Air loads the private `.env` copy.
- `internal/testdb/`: integration-only isolated PostgreSQL schemas, one schema-scoped SQL pool per test shared by GORM and SCS.

Both public and management link flows use the same link store; management also receives the student store backed by the same database. Enrollment belongs to the link store's atomic SQL command, not a separate student-store transaction. Both stores take the live feed's `Changed` callback (nil in tests). Dependencies are passed explicitly; there is no global database or `init()` setup. Folder separation is not an authorization boundary.

## Cryptographic link design

Each token uses 32 bytes from Go's `crypto/rand`: 256 bits of OS-backed randomness, encoded as 43 URL-safe Base64 characters without padding. Tokens contain no customer data, timestamps, or sequential IDs and are not JWTs.

PostgreSQL stores the SHA-256 token digest, contact snapshot, lifecycle timestamps, staff audit, nullable `terms_accepted_at`, accepted `program_start_date`, and local `payment_plan`. Contact submission inserts one Student with a unique source digest, matching normalized details, and the same submission time. Student contact edits do not rewrite Link snapshots. Email confirmation and usable tokens are never stored; issuance returns a URL only after persistence succeeds.

SHA-256 is the lookup digest, not the source of randomness. Go 1.26's `crypto/rand.Read` terminates the process if the OS random source fails; there is no weaker fallback.

Customer reads and guarded writes hash the supplied token and check for a matching database record that is neither canceled nor expired, using bound SQL parameters. Expiry is compared with the database clock (`expires_at > now()`) on each database operation, so it is a hard cutoff: a customer mid-form whose link lapses gets the 404 page on their next autosave or submit, and keeps what was already saved. Only GET records `opened_at`; HEAD is read-only, and POST never records a visit. Repeat GETs of an already-opened link are SELECT-only. The first GET conditionally stamps and returns the first-visit row; if a concurrent open, cancellation, or expiry prevents that update, the store rereads usability rather than returning stale access. Multiple issued tokens remain valid until canceled or expired; opening a link does not consume it. A database error does not grant access. Saving a contact field updates only the matching, usable, unsubmitted link; it cannot insert or revive a canceled or expired link.

`HEAD /customer/{token}` is a read-only usability check: HTTP 200 even after final submission, 404 for unknown, malformed, canceled, or expired tokens, and 500 for lookup failures. It returns no body, keeps the normal customer response headers, and never changes `opened_at`, saved details, or submission state. A later GET still records the first visit. GET-based previewers can therefore mark a link **Opened**; this is not proof of human interaction.

Tokens are bearer credentials: anyone possessing a usable URL can open it and edit its unfinished contact fields without staff authentication until the form is submitted. Management routes require a current staff session, while generated-link and customer responses use `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. Login, management writes, and customer submissions retain Go's native cross-origin protection.

### Welcome and student contact form

The welcome introduces student information, agreements, and payment in Puerto Rican Spanish. Its copy/logo remain unchanged. **Próximo** is an ordinary navigation link; the welcome loads no JavaScript. Welcome/form GETs record the first visit, never a submission. Returning before submission preserves contact drafts. Contact submission advances to the agreement; actual payment collection remains deferred.

The form uses `lang="es-PR"` and the heading/title **Información del estudiante / Información del estudiante | Masterminds**. Its five required controls appear in this order: **Nombre completo** (`name`), **Correo electrónico** (`email`), **Confirmar correo electrónico** (`email_confirm`), **Número de teléfono** (`phone`), and **Municipio** (`municipality`). Customer errors/statuses and confirmation are Spanish; management labels and parser errors remain English.

The shared server-side rules are:

- **Full name:** validate UTF-8, trim surrounding whitespace, and collapse internal Unicode space separators to one ASCII space. Preserve case, accents, and decomposed accents. Accept Unicode letters and combining marks attached to a preceding letter in the same word. Final submission and canonical editing require at least two words; a one-word draft is allowed. Reject digits, punctuation (including apostrophes and hyphens), markup, emoji, internal controls/NUL, invalid UTF-8, and words beginning with a combining mark. Invalid characters are rejected, never silently stripped.
- **Email:** trim and require one bare ASCII mailbox, at most 254 bytes total and 64 bytes in its local part. `net/mail.ParseAddress` must return that same address without a display name. Require a dotted DNS-style domain with nonempty labels of at most 63 ASCII letters/digits/hyphens and no leading/trailing hyphen. Lowercase only the domain, preserving local-part case, plus tags, and dots; ASCII punycode domains are accepted. Reject display names, lists, comments, quoted local parts, address literals, internal whitespace/controls, Unicode mailbox text, dotless domains, and malformed labels. This is strict syntax checking, not deliverability or ownership verification: no outgoing email, DNS/MX/SMTP probes, or provider-specific typo guessing.
- **Email confirmation:** parse by the same email rules, then compare normalized addresses. Surrounding whitespace and domain case do not matter; local-part case does. Confirmation is transient: no column, draft endpoint, Students field, or autofill from saved email. Reload restores saved email but requires confirmation again; invalid final attempts retain the attempted confirmation for correction. Paste remains allowed. Matching values do not prove mailbox ownership.
- **Phone:** trim and accept ASCII digits, spaces, parentheses, hyphens, dots, and one optional leading `+`. Parse with default region `PR`, then require the pinned phone metadata to identify a valid `PR` or `US` number. Local ten-digit, leading-1, and `+1` formats normalize to stored/displayed E.164, such as `+17872345678`. Reject incomplete/metadata-invalid numbers, letters/vanity numbers, extensions, Canada and other NANP regions despite `+1`, and other countries. Municipality never constrains phone region: a Puerto Rico resident may use a valid US number. No SMS verification, carrier lookup, or network call occurs.
- **Municipality:** trim and require exact membership in the shared Spanish option list: all 78 Puerto Rican municipalities alphabetically, followed by **Fuera de Puerto Rico**. The empty **Selecciona tu municipio** prompt is not one of the 79 values. No country/address follow-up is collected. Forged or alternate-spelling/case values fail; an invalid attempted option renders the empty invalid prompt, never an injected option or a silently chosen municipality.

Every persisted-field parser permits an empty draft to clear that field; nonempty drafts must still satisfy its rules. All four fields are required for final submission and staff edits. Customer request bodies remain limited to **4 KiB of encoded bytes**, not name characters, with a five-second handler database context.

#### Isolated drafts and final submission

Each persisted control sends only its own named parameter to `POST /customer/{token}/draft/{field}` on HTMX `change`, where `field` is exactly `name`, `email`, `phone`, or `municipality`. A known field requires exactly one same-named POST value: an explicit empty value clears it, missing/duplicate values return 400, and unrelated parameters are ignored. Unknown keys return the branded 404. No unkeyed draft endpoint or confirmation autosave remains.

`SaveDraft(ctx, token, key, value)` updates only that allowlisted column, never a whole-form snapshot. Equivalent normalized values, including an empty replay, return **Guardado.** without an UPDATE or live event. Different-field requests cannot overwrite one another; each control queues only its latest pending change. A successful draft persists across reloads without setting `submitted_at` or creating a student, replying only inside `#save-status-<field>` with **Guardado.** or a Spanish **No se guardó.** error; it never redraws a control. Invalid drafts preserve the last saved value. Any nonempty saved field means **In progress**; clearing the last one restores **Opened** or **Not opened** according to visit history. Text not sent before leaving, or changed while offline, is not persisted. Network failure shows **No se guardó. Verifica tu conexión e intenta de nuevo.**, preserves input, and does not label the field syntactically invalid.

**Enviar información** posts all five named controls to `POST /customer/{token}`. The server validates all four required contact fields plus required confirmation, collecting every field error before any write. Browser native validity and the form-only script provide mismatch feedback, but identical Go validation remains authoritative for ordinary no-JavaScript POSTs. Correcting either email clears stale mismatch validity; accessible server errors remain associated with their fields, and a final HTMX 422 focuses the first invalid control.

Both customer POST paths cap, parse, and validate before database access. Valid input calls the intentionally distinct `SaveDraft` or `Submit(ctx, token, students.Details)` directly, without an HTTP-layer preflight or post-save lookup. The store returns authoritative `links.SaveResult`: `Saved` means a committed write or a usable unfinished draft already matching the normalized value; `AlreadySubmitted` means the still-usable link was already finalized; `Unavailable` (zero) means malformed, unknown, canceled, or expired. Database errors return `Unavailable` plus an error, never a successful outcome.

`Saved` returns draft status-only 200 or advances the completed contact form to `/agreement`; `AlreadySubmitted` makes the same advancement, or returns stale-draft 409. Advancement uses ordinary 303 or HTMX POST 200 with `HX-Redirect`, always without a response body. `Unavailable` returns 404; database failures return retryable 500. Malformed, oversized, or invalid input still observes usability/completion before input errors: unusable links return 404 and submitted forms advance without replacement. Only usable unfinished forms receive 400 for malformed input, 413 for oversized bodies, or 422 for validation. Parse errors restore saved details; validation/save errors retain attempted controls and confirmation.

HTMX contact errors return `customer-response` with the stable `#customer-content` section; 422 swaps field errors rather than generic failures. Success navigates to the complete agreement page using `HX-Redirect`. Ordinary submission uses 303 and works without JavaScript; blur autosave requires JavaScript. Responses retain `Vary: HX-Request`; HTMX 404/409 requests a full refresh. Detached drafts cannot overwrite a replacement form or later page.

A successful contact submission uses one PostgreSQL data-modifying CTE: guarded Link UPDATE returns normalized details/digest/time directly into the Student INSERT. It commits the snapshot, `submitted_at`, exactly 168 hours of continued enrollment access, and one matching Student atomically. Concurrent attempts cannot mix answers or enroll twice. Insert or commit failure rolls everything back, preserving the draft and initial expiry for retry. Only database success advances to the agreement; contact submission is not agreement acceptance, verified identity, or payment.

Submitted welcome/form GETs and repeated final POSTs advance to `/agreement` until access expires. Redirect bodies contain no contact values; agreement/payment views never receive contact details. Replays never replace answers, enroll again, or move deadlines. Stale keyed drafts return empty 409, with `HX-Refresh` for HTMX. If cancellation wins first, no student is created; if contact submission wins, cancellation cannot revoke it. HEAD remains read-only with no body.

### Agreement and local payment choice

`GET /customer/{token}/agreement` requires completed contact information. It renders the official eight-clause Spanish agreement from `PAYMENT_AGREEMENT_PLAN.md`, including the current program date in its heading and clause (2). An unconfigured date shows an explanatory error and no acceptance control.

The native required checkbox posts `accept_terms=yes` and the displayed `program_start_date`. Server validation is authoritative. `AcceptTerms` requires a usable submitted link and atomically compares the displayed date with the locked settings row before saving database time and its fixed date snapshot. Unaccepted agreements preload the server-trusted date before parsing POST; generic save errors retain it for an unchecked retry even after the five-second context expires. Only a stale-date rejection rereads settings, returning 422 with the new agreement and unchecked consent; a settings-load failure withholds consent instead. Replays preserve the first timestamp/date, even after staff changes settings. Accepted agreements remain reviewable from their immutable snapshot without reading settings or accepting again.

`GET/POST /customer/{token}/payments` requires persisted acceptance; direct requests cannot skip contact or consent. The three choices are **Pago Único** ($600.00), **Plan de 3 Pagos Mensuales** ($625.00: $208.34, $208.33, $208.33), and **Plan de 6 Pagos Mensuales** ($650.00: $108.35, then five $108.33 payments). Every amount/date is visible before choosing. Schedules use the current Puerto Rico calendar date (UTC−4, `America/Puerto_Rico`); each future month clamps the original day to its last available day, restoring the original day in longer months. All calculations use integer cents.

The schedule is explicitly for a first payment made today; paying another day will change it. A single allowlisted `payment_plan` POST saves the local choice, redirects to the page, and permits changes. Missing/forged choices return 422; duplicate choices return 400. Failed changes retain the attempted radio selection while labeling any prior saved plan separately and stating that the attempt was not saved. Failed first saves show no saved-plan notice. The saved status explicitly says no charge occurred. Both pages use ordinary forms without JavaScript, the existing 4 KiB limit/five-second database context, no-store/no-referrer headers, token expiry, and read-only HEAD.

Student `is_active` defaults true; inactive students and their source links are retained, never deleted by deactivation. No automatic deactivation, reinstatement fee collection, Stripe link, checkout, subscription, payment confirmation, or charge-protection workflow is implemented in this slice.


### Students details and field editing

The Students table shows **Name**, **Email**, **Phone**, **Municipality**, and **Submitted**, ordered newest completion first. Its name anchor supports Tab/Enter and ordinary navigation; pointer clicks elsewhere on the row activate it without replacing table semantics or interfering with selected text or other controls. With HTMX and JavaScript, details load into a centered native `<dialog>` with Close, Escape, and native focus containment. Closing resolves the initiating row anchor again and restores focus even after a save replaced that row. Without JavaScript, the same anchor opens a full detail page with **Back to students**, and pencil links, Save forms, and Cancel links still work.

Each pencil opens only its addressed field, applying the same required name/email/phone/municipality parser as final customer submission. Staff edit email directly without confirmation. Municipality uses a native required select with the same 79 options, an empty **Select a municipality** prompt, and saved selection; an invalid attempted value selects the empty prompt with an inline error. Save updates exactly one registered canonical column using a bound value, never another field/student or the original link snapshot; even a canonical phone edit leaves Customer links/Trash phone unchanged. An HTMX modal save returns the field plus an out-of-band table row, while standalone saves redirect to the full detail page. Validation returns HTTP 422 with the attempted value and accessible error, ready for correction; unknown fields or missing students return 404, malformed/oversized bodies return 400/413, and database failures return 500 rather than a saved notice. Field requests queue within the details panel. Other HTTP or network failures retain the current table, saved values, and unsaved input with a nearby retry message; expired staff sessions redirect to login instead of injecting login HTML into a field. Cancel reloads the saved field, including a select's saved option.

Fields come from the ordered, package-owned `students.Field` registry, not an interface, reflection, schema discovery, JSON/EAV storage, or arbitrary-column writes. Its four entries are Name, Email, Phone, and Municipality. The concrete struct carries `Key`, `Label`, `InputType`, `Required`, `Options []string`, `Value func(Student) string`, and `Parse func(raw string, required bool) (string, error)`; callers treat the registry/options as read-only. Templates iterate it for table headings/cells and details/editors, rendering an input or native select according to `Options`. Adding a field requires its typed Details column, metadata/parser, and explicit customer collection and atomic snapshot changes; the registry alone does not expand the customer wire contract.

### Testing the customer contact form

Use disposable links and test fixtures only; never send email/SMS or call example numbers.

1. Check welcome and form at 320×568, 390×844, and 1440×900. Welcome copy/logo and **Próximo** remain unchanged and script-free. The form should have one column, readable labels, visible focus, touch-sized native controls, exactly 79 selectable locations plus the empty prompt, accented options, and normal vertical scrolling without horizontal page overflow.
2. Enter **María José Pérez Rivera**, **Student.Name+tag@EXAMPLE.COM**, **(787) 234-5678**, and **Río Grande** in different orders. Inspect draft requests: each must contain only its own named parameter. Clear email and confirm an explicit `email=` changes no other saved field; restore it, reload, and check normalized email/phone with blank confirmation. Try **(202) 555-0123** while keeping a PR municipality, then **Fuera de Puerto Rico** with a PR phone; both residence/phone combinations are valid.
3. Type a mismatching confirmation; final submission must be blocked in the browser. Correct either address and check stale mismatch text/custom validity clears. Use a browser-valid but server-invalid name such as **María-Pérez Rivera** to force a real 422: all five controls and selection must survive, Spanish name error stays intact, and focus reaches Name. Restore the valid full name and matching confirmation, preserving local-part case.
4. Disconnect while autosaving and submitting: Spanish network feedback must retain current input without calling a valid field invalid. Restore connectivity and retry. With JavaScript disabled, invalid submission preserves the full error form and valid submission advances to the agreement.
5. Submit valid details: the browser reaches `/agreement`, with no contact values or confirmation in its HTML. Inspect matching normalized Link/Student rows and exactly one enrollment. Replayed submissions and both old GET URLs must advance without changing details/deadlines; stale keyed drafts return 409.
6. Keep Customer links and Students open during these changes. Link drafts must update displayed name/phone while retaining its one-time URL and **Copy link**. Email and municipality must never enter main/Trash HTML. Canonical staff edits must update Students only, not its source snapshot or registry phone.

### Testing agreement and plan selection

1. Save **Fecha de comienzo** above the registry. After contact submission, verify all eight clauses and the same Spanish date in the heading and clause (2). An unchecked checkbox must block native submission; an unchecked direct POST must return 422. Payment requests before acceptance must redirect to the agreement.
2. Leave an agreement open, change the management date, then accept the stale page. Verify 422, updated date, unchecked consent, and no saved acceptance. Accept again; inspect the fixed date/timestamp. Change management again: review/replay must preserve the accepted snapshot.
3. Inspect all three totals, counts, dates, and installment amounts at 320×568, 390×844, and 1440×900; no horizontal overflow or hidden schedule. Try saving without a choice and forging a plan. Save/change/reload each legitimate choice; no charge or checkout should occur.
4. Repeat contact submission, consent, and plan saving with JavaScript disabled. Verify source/student retention, private-free new pages, read-only HEAD, replay safety, and expired-token rejection. Automated schedule regressions cover PR midnight, leap years, month-end restoration, and year rollover.


### Testing cancellation

Sign in and create three links. Open Students and all three customer URLs. Submit complete matching contact information through the first: it advances to the agreement and Students gains exactly one record. Save a draft name/phone through the second; reload and confirm no student or confirmation was saved. Leave the third on welcome. Live registry rows must show **Finished**, **In progress**, and **Opened**, retaining one-time URLs until reload. Finished means contact submission, not agreement acceptance or payment; that row has no cancel button.

Cancel the unfinished **In progress** row, first dismissing and then accepting the confirmation dialog. The row must leave the main list and appear in Trash with its draft/audit, name/phone only, and no cancel button. Other unfinished URLs and completed enrollment pages remain usable. Opening or submitting the canceled URL returns branded 404; canceled HTMX requests refresh the entire page.

To check that cancellation keeps unrelated URLs, create two links without reloading, then cancel the first. It disappears from the main page, while the second row keeps its URL and **Copy link**, which still copies a working customer URL. A status change on another row, such as its customer opening it, appears without a reload, and that row keeps its URL too.

With clipboard completion deliberately delayed for the second link, cancel the first link before clipboard work settles. The second link stays usable; delayed clipboard success must not show the copy toast, and delayed failure must not overwrite **Link canceled.** Any newer staff action invalidates earlier clipboard feedback, including a visible copy toast, when it starts, even before its HTTP response arrives; background list refreshes do not.

**Cancel all unfinished links** revokes usable unfinished links across tabs/accounts. Expired rows stay expired; submitted links, enrollment access, students, and prior audits remain unchanged. The live list removes canceled rows while preserving other shown URLs. A newly created link still opens welcome and an empty contact form.

Cancellation never deletes rows or saved drafts; Trash retains them. Repeating cancellation is safe and keeps the first canceler. Cancellation is not a lifetime or expiration setting.

### Testing expiry

Change the Settings slider to three days and save; confirm audit and new-link expiry, with older unfinished deadlines unchanged. Submitting a contact form must replace its deadline with seven days of continued enrollment access, independent of the slider.

For disposable local data only, set an unfinished link's `expires_at` to the past in PostgreSQL while its customer page is open, then change a contact field: the next autosave switches to the 404 page. Reload management: the link is absent from **Customer links** and appears in **Trash** as **Expired**, with its pre-cutoff draft retained, only name/phone projected, and no student.

For disposable mock data, age a completed link's `submitted_at` and matching Student `created_at` to 25 hours ago, retaining seven-day expiry. Reload management: the link leaves the main list without entering Trash; Students and the agreement/payment flow remain accessible. Move `expires_at` to the past: every customer entry point returns 404, but its student and agreement snapshot remain retained.

### Testing Students

Submit two links with the same complete details and check that Students shows two separate records with all four contact columns. Open one by pointer and again with Tab/Enter; test Close and Escape, including focus returning to the initiating anchor. Use each pencil: reject a whitespace-only or one-word name, malformed email, invalid/Canadian phone, and forged municipality with server-side 422. Correct to a valid Unicode full name, plus-tag email, PR/US phone, and an accented municipality or **Fuera de Puerto Rico**. Each save must update its detail and table cell without closing the dialog, normalize phone to E.164, and survive reload. Other canonical fields, the other student, and the original four-field link snapshot must stay unchanged; canonical phone edits must not change registry phone. Reopen text and municipality editors, change values, then **Cancel**: saved text/selection must return. Staff email editing has no confirmation field.

With a field editor open and unsaved text present, block its request or disconnect the browser and try Save/Cancel: a nearby error must appear without losing the input, saved table row, or existing details. Restore the connection and retry. Repeat the edit/save/cancel flow with JavaScript disabled through the full detail page. Check desktop and mobile layouts, table scrolling without whole-page overflow, dialog internal scrolling, and visible keyboard focus. Keep link clipboard/cancel checks separate from Students: its pages must not load link-only clipboard behavior.

## Verification

Integration tests use a real, dedicated PostgreSQL test database. Create it once:

```fish
docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test
```

Run the checks:

```fish
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
go test -race ./internal/logging .
go test -race -tags=integration ./...
go vet -tags=integration ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Each integration test creates its own PostgreSQL schema through one schema-scoped pool shared by GORM and direct SQL/SCS access. Cleanup closes the HTTP server, stops auth session cleanup when present, drops only the generated schema, then closes its pool. Regression contracts include:

- Shared name/email/phone/municipality parser boundaries, normalization, the exact 79-option list, required versus optional drafts, and transient email confirmation/matching.
- Digest-only storage and token boundaries; status progression from any populated field, field-isolated and concurrent drafts/explicit clears without enrollment, independent links, hard access cutoffs, and verified staff attribution for creation/cancellation.
- Source-link foreign-key protection against deletion/hash changes, and repeated current-model `AutoMigrate` preserving links and students.
- Exactly one coherent four-field enrollment under concurrent final submissions; rollback of every contact field/completion/expiry on student insert or deferred commit failure; successful retry; submission/cancellation races with either completion plus one student or cancellation plus none.
- Completed GET/final-POST replays return HTTP 303 (HTMX POST: HTTP 200 with `HX-Redirect`) without answer/deadline changes or contact/confirmation disclosure, including invalid-body replay precedence, stale keyed-draft 409, retryable customer fragments retaining all attempted controls, and completed-access expiry without student loss.
- The exact 24-hour completion boundary, next-render main-page archival, no completed records in Trash, stale HTMX cancels that only acknowledge while the refreshed list leaves archived rows out, and Students retention after source expiry.
- Student newest-first reads, all-field validation/allowlisting and escaped rendering, one-column canonical edits isolated from other fields/students and the four-field source snapshot, native municipality select/cancel/error behavior, full-page/HTMX editor and modal/OOB-row contracts, rejected requests without mutation, and honest empty/error responses.
- Name/phone-only personal-data projection in main/Trash rows, with email/municipality absent even when populated, and canonical phone edits isolated from registry phone.
- Real management authentication/CSRF boundaries including Students, show-once URLs whose elements match their rows' preserved slots, notice-only HTMX actions that trigger a list refresh, rows-only link and record refreshes, first-canceler preservation, missing-ID no-swap behavior, database failures without success fragments, and unfinished-only single/bulk cancellation.
- Settings default, store/database 2–14 day constraints and singleton row, changes affecting only new unfinished access; authentication credential failure, rate limiting, token rotation, persistence/expiry, password/revocation/removal invalidation, username recreation, browser-local sign-out, and public customer access bypassing staff-session storage.
- Production logging privacy/classification, including name/email/phone/municipality/confirmation exclusion, first-failure/leaf-route ownership, actual SCS commit status, bounded exact slow-query counts under concurrent flushes, and silent successful/expected requests.
- Expired-only session cleanup with safe failure/cancellation handling, plus real-listener graceful and forced shutdown with the unchanged ten-second drain and no signal-triggered request cancellation.
- Live updates: one catch-up event per connection, change events past the server's `WriteTimeout`, streams ending once shutdown starts (late connections included), and link-store writes announcing exactly the changes they commit.

Customer HEAD regressions use real HTTP connections and direct persisted-row comparisons: fresh HEAD leaves **Not opened**, GET records the first visit, later HEAD preserves timestamps and draft/submitted data, unusable tokens return 404, and lookup failures remain 500. They check body suppression on the wire, customer response headers, and unsupported-method rejection. Separate link-store regressions protect the populated first-GET result, unchanged repeat-open timestamp, and cancellation winning while the first-open update waits. POST never records a first visit.

These checks do not prove randomness statistically. Cryptographic randomness comes from the standard library's OS-backed generator, not from a finite collision test.

Browser smoke checks complement those server/database regressions: native dialog/focus behavior after an OOB row replacement, field validation/cancel/retry and intercepted-network input retention, clipboard isolation/sequencing, live updates across tabs and staff with URL and focus retention through refreshes and reconnection after a restart, the next-load archive cutoff, and desktop/mobile layout need a real browser. They are not proved by HTML string assertions alone.

## Agreed first-part requirements (target workflow)

The following describes the future customer/invoice/payment workflow, not additional implemented behavior. The Students archive above is per completion and does not yet supply this workflow's unique customer identity or invoice binding.

### Management and link lifetime

Management enters a customer's phone number and generates an invoice link. Link lifetime is configured separately on the **management settings page**, so it does not need to be entered for every generation.

Settings persist in PostgreSQL. The lifetime slider runs from 2 to 14 days in one-day steps, defaulting to 7. Issued links retain their initial deadline when settings change. Contact submission replaces it with seven days of continued enrollment access; acceptance and local plan changes never extend it. Future invoice/payment transitions must define their own behavior.

### Customers, invoices, and links

These are separate concepts:

- **Customer:** has an immutable internal `customer_id`. Phone is a normalized, unique lookup for the management workflow, not the primary key or proof of identity. Include the country code when normalizing numbers.
- **Invoice:** represents the existing payable obligation and references its customer.
- **Link:** grants entry to that invoice's customer flow. It references the invoice, which identifies the customer.

Generation finds or creates a minimal customer record using the normalized phone number. Remaining customer information is collected through the forms and fills in that existing record.

Generating another link for the same customer in this workflow must reuse the existing invoice. It must not silently create another invoice or debt. This does not establish a permanent one-invoice-per-customer rule for future billing workflows.

Multiple links can be active at the same time. Different customers have separate links, records, and form state. If multiple links point to the same invoice, they all share its payment state. Whether reissuing a link revokes older links is still undecided.

### Link generation and destination

- Every generation produces a fresh, cryptographically random URL token, even for the same customer and invoice.
- Store the token's hash rather than the usable token. Public URLs must not contain customer phone numbers or other personal information.
- Links use one trusted, configured destination base with different internal paths, not arbitrary external redirect destinations.
- Both management and customer experiences are owned by this project; a separate service is not required.

### Expiration and the customer flow

- Check expiration server-side using an absolute expiration instant, not the browser's clock. PostgreSQL expiration fields use `timestamptz`.
- An expired link shows the same customer-facing 404 page as an unknown or canceled link.
- Expiration is a hard cutoff: it applies to every request, including a customer already moving from form to form. Reloading or re-entering through an expired link shows the 404 page; automatic renewal or recovery is not required for this scope. The customer can contact the company.
- Expiration does not delete customer information already saved.
- Correct expiration does not depend on a background deletion job.

Link validity and invoice payment state are different checks. An accepted form flow does not bypass payment-state checks later.

### Forms, storage, and API

Customer forms submit to this application's backend API. The server validates and saves customer information in PostgreSQL; management can retrieve it through authorized backend operations.

Forms update the customer resolved from the link and server-side flow context. A phone number or customer ID submitted by the browser must not be allowed to select a different customer's record. Customers must not be able to view or modify one another's information.

HTMX endpoints can return HTML fragments. A separate JavaScript frontend or duplicate JSON endpoint for every UI action is not required. Management and customer areas do not need webhooks to communicate within this application.

Management now uses CLI-managed staff username/password accounts and persistent sessions. Customer-verification mechanisms for the broader invoice/payment workflow have not been selected.

### Preventing duplicate payment

Payment implementation comes later, but this invariant already applies: **one invoice must not receive a second successful payment through checkout**.

- Payment state belongs to the invoice, not the URL, phone number, or browser session.
- Once paid, every link and payment entry point for that invoice must stop offering payment, including already-open forms.
- Concurrent requests must atomically create or resume one active payment attempt rather than create competing charges.
- Retries of the same attempt must reuse the provider's payment object or idempotency key. A timeout is not proof that a charge failed.
- Uncertain attempts remain pending until reconciled with the provider; they must not immediately permit another charge.
- Payment confirmation must come from verified provider information, not a browser success page. Repeated provider notifications must be harmless.
- Database transactions must not remain open during payment-provider network calls. Database checks alone cannot prevent a provider from charging twice.

Stripe is the planned payment provider but is not integrated. Verified provider events, fixed-count billing, payment confirmation, duplicate-charge prevention, and automatic 14-day deactivation require that later integration; local agreement acceptance and plan selection do not provide them.

## Implementation approach

The current slice implements link issuance, validation, unfinished-only cancellation, isolated contact autosave, transient confirmation, atomic contact submission/enrollment, retained canonical Students editing, live management updates, program-date configuration, immutable agreement acceptance, and local three-option payment selection. Keep later changes reviewable for correctness, security, readability, and maintainability.

Implement complete, runnable slices rather than disconnected handlers or pretend functionality. Add database constraints and focused regression checks for critical contracts as the relevant behavior is implemented, including customer isolation, expiration boundaries, invoice reuse, and concurrent payment attempts.

Unique customer/invoice binding, actual Stripe checkout/billing, verified payment events, customer verification, and link-reissue policy remain deferred. Valid contact syntax, agreement acceptance, and a saved payment choice do not verify identity or constitute payment.
