# PaymentWebsite

A company payment application with separate management and customer interfaces, one shared Go backend, and PostgreSQL persistence through GORM. The target workflow uses customer-specific invoice links; shared customer links are not part of the design.

## Current status

The runnable slice implements CLI-managed staff access, persistent sign-in, hash-only customer links with progress status and configurable expiry, clipboard copying, cancellation, and an autosaving customer name form that locks on submit:

- `/` redirects to `/management/`. Signed-out staff see a login form; signed-in staff see a dark workspace with **Customer links** and **Settings** navigation, their username, and **Sign out**. The shared link registry shows status badges and an empty state; navigation moves above the content on small screens. Only existing link and settings workflows are exposed.
- **Settings** (`/management/settings`) has a slider for how long new customer links stay usable: 2–14 days in one-day steps, 7 by default. Saving records who changed it. Each link's expiry is fixed when it's created, so moving the slider never changes links already sent.
- Staff accounts are created through the CLI, not a registration page. Passwords use Argon2id; SCS stores sessions in PostgreSQL for a fixed 14 days. Password changes, revocation, and account removal invalidate existing sessions.
- **Create & copy link** appends and copies a new URL. The URL is shown only in that creating response: PostgreSQL stores just its digest, so it can't be recovered later. A lost URL is replaced by creating a new link.
- Every staff account sees the same saved list after reloads and server restarts. Rows show the customer's saved name, creation and expiry times in the viewer's time zone, and a status: **Not opened**, **Opened** (first visit recorded), **In progress** (autosaved data, not yet submitted), **Finished** (customer pressed **Submit**), **Expired** (lapsed before the customer finished), or **Canceled**. Once payments exist, **Finished** will mean a completed payment. The list shows every link ever issued, without paging: deliberate for two staff and small mock data, and it needs pagination before history grows.
- `POST /management/links` persists the random token's digest, then returns a row fragment for HTMX or the complete list for ordinary form submissions.
- `POST /management/links/{id}/cancel` revokes one link by its management ID; `POST /management/links/cancel` revokes every link still usable when its database update runs, so links created afterward, from any tab or account, stay active. Revocation keeps the row and submitted name visible as **Canceled**; there is no deletion UI. Expired links can't be canceled and keep showing **Expired**.
- An HTMX single cancel replaces only its own row, showing the link as PostgreSQL now holds it: saved name, creator, and first canceler kept, and **Expired** if it lapsed. Other rows are untouched, so a newly created row keeps its URL and **Copy link**; their statuses refresh on the next reload. An unknown or malformed ID answers HTTP 204, so nothing is swapped, and a failed cancellation answers HTTP 500 without a row or success notice. Ordinary form submissions, **Cancel all**, reloads, and navigation render the complete list, which drops any one-time URLs still on screen.
- While a create or cancel request is in flight, the page disables its buttons, so the normal UI sends one change at a time. This is not request serialization: other tabs, other staff, and direct requests can still interleave, and each is applied on its own.
- Each row is an audit trail: "created … by gabriel · canceled by worker". The username comes from the verified server-side session, never from form input, and the store refuses creation or cancellation without one. **Cancel all** records its canceler only on links that were still active. Links from before auditing show no names.
- `/customer/{token}` remains public to anyone holding an issued URL. Unknown, modified, canceled, and expired tokens all return the same branded HTTP 404 page. Its name form autosaves when the customer leaves a changed box and finishes on **Submit**, which locks it: afterwards the customer sees a read-only confirmation and can't change their answers.

Links are not customer- or invoice-bound yet; the name still belongs to an issued link. Full customer records, invoices, and payments remain outside this slice. Sign-in expiry does not expire customer links.

**Test/development only:** management is authenticated, but invoice and payment workflows are not implemented. Use disposable data, never real customer/payment data.

## Stack and development

- Go 1.26 or newer, using `net/http` and `html/template`; `go.mod` selects the patched Go 1.27.2 toolchain.
- HTMX for server-rendered interactions; mobile-first pages.
- PostgreSQL for persistence, accessed through GORM and its PostgreSQL driver.
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

`DATABASE_URL` is required in both server and CLI modes. `SESSION_COOKIE_SECURE` defaults to true when unset. GORM connects and runs `AutoMigrate` for links, users, settings, and the SCS-compatible sessions table on startup. The links migration also drops the `token_ciphertext` column left by the removed encrypted URL recovery, and backfills columns it adds to existing rows, once, when each column first appears: existing named links are marked submitted, since names were only saved by submitting before autosave, and existing links expire 7 days after creation. Models define the tables and constraints; there are no versioned migration files.

Restart an existing Air process after changing its environment so it reloads `.env`. Keep one instance running. Without Air, export the required variables before `go run .`; the application never loads `.env` itself or silently falls back to local credentials.

Use one server or Air instance at a time: they share port 3000, and Air instances also share `tmp/main`. Keep the Air dev server running and reuse it (see `AGENTS.md`).

Open <http://127.0.0.1:3000/management/> and sign in using a CLI-created account. **Create & copy link** copies the same-origin URL and appends it; **Copy link** on that new row recopies it until the page reloads or **Cancel all** refreshes the list. Clipboard access requires JavaScript and HTTPS or localhost. If blocked, the new URL remains visible for manual copying; failed creation leaves the clipboard unchanged.

Air watches Go, HTML, CSS, and JavaScript files. Its generated defaults do not automatically refresh the browser; refresh manually. Use `go tool air`, not a separately installed global `air` command. The existing `.air.toml` was generated with `go tool air init`.

Both interfaces load the shared `static/styles.css` stylesheet from `/static/styles.css`. Customer brand colors are defined on `:root`; management has a scoped dark palette and native dark controls, including sign-in and settings, regardless of the system color preference. Customer rules use `.customer` for the name form and `.customer-not-found` for the invalid-link page. Go embeds the stylesheet, the management clipboard script (`static/management.js`), and the existing Masterminds logo (`static/logo.png`) into the binary and serves them under `/static/`, so deployment needs no separate asset directory. CSS and JavaScript edits require a rebuild, which Air handles during development.

No frontend build step is required. Air's generated `tmp/` directory is ignored by Git.

## Staff accounts and sessions

Create the two staff accounts explicitly. For the default local database:

```fish
set -gx DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite?sslmode=disable'
go run . user add gabriel
go run . user add worker
go run . user list
```

Replace example usernames as needed. Usernames are case-insensitive, normalized to lowercase, and accept 1–64 ASCII letters, digits, `.`, `_`, or `-`. Passwords require at least 4 Unicode characters and at most 1024 bytes, entered twice at a hidden terminal prompt. Password arguments, environment variables, and piped plaintext are not accepted. Account commands need `DATABASE_URL`, but not a running web server.

The four-character minimum is temporary for mock-data development; raise it before production.

`user passwd <username>` changes a password and invalidates prior sessions. `user revoke <username>` invalidates sessions without changing the password; `user remove <username>` removes the account, not its links. Recreating a removed username cannot revive old cookies. Duplicate creation never overwrites an existing password.

Both staff have the same access to the link registry; the separate accounts exist so each action records who did it. Sessions contain account identity/version, not link history. Persistent HttpOnly, SameSite=Lax cookies remember sign-in across tabs and browser restarts; ordinary visits do not extend the 14-day deadline. Production cookies are Secure even behind Railway's TLS proxy. Login attempts from every client share a per-process limit of two initial attempts, then one every 12 seconds. This bounds password guessing and Argon2 memory use; the accepted tradeoff is that anyone who keeps the limit spent blocks new staff sign-ins until they stop, while existing sessions keep working. There is no registration or email recovery.

**Sign out** posts to `POST /management/logout`, which deletes that browser's session row and expires its cookie; the account's other browsers stay signed in. To end every session for an account, use `user revoke`.

## Railway test deployment

The `PaymentWebsite` service in the existing `amused-light` project deploys `devGabriel131/PaymentWebsite`, branch `master`, using Railpack. Railway names this environment `production`; the application is still for testing only.

Service configuration:

- `DATABASE_URL=${{Postgres.DATABASE_URL}}` references the project's PostgreSQL service over Railway's private network. Local `.env` credentials are not uploaded or reused.
- `LINK_ENCRYPTION_KEY` is no longer read. After the first deployment that drops `token_ciphertext`, delete the variable and destroy any backup of it; without the key, older database backups that still contain ciphertext can't yield URLs.
- `SESSION_COOKIE_SECURE=true` keeps management cookies HTTPS-only.
- `PORT=8080` makes the application listen on all interfaces for Railway routing. Without `PORT`, local startup is bound to `127.0.0.1:3000`.
- The health check remains `/management/`: signed-out requests receive HTTP 200 with only the login form. PostgreSQL connection and migrations must succeed before HTTP starts.
- Railpack uses the preferred Go toolchain declared in `go.mod`; the deployed process does not run Air. Pushes to `master` trigger automatic deployments.

For a manual deployment from this directory:

```fish
railway up --project a0207263-4a1b-4174-b693-5830fd46e486 --environment production --service PaymentWebsite --ci -m "Update PaymentWebsite"
```

The public HTTPS domain serves both management and customer routes. PostgreSQL traffic stays private. Do not copy Railway database credentials into `.env.example` or commit a local `.env`.

Manage hosted accounts inside the deployed app container, where the private PostgreSQL URL works:

```fish
railway ssh --project a0207263-4a1b-4174-b693-5830fd46e486 --environment production --service PaymentWebsite
```

Then run these in the remote shell:

```sh
/app/out user add gabriel
/app/out user add worker
/app/out user list
```

Railpack currently builds `/app/out`; account commands ship in that same binary. Real staff enter their own passwords at the hidden prompts. The same remote commands support `passwd`, `revoke`, and `remove`. `railway run` and `railway shell` execute locally and do not make Railway's private database hostname reachable; do not expose PostgreSQL publicly or copy production credentials into local `.env` for this CLI.

## Project structure

```text
.
├── main.go
├── static/
│   ├── logo.png
│   ├── management.js
│   └── styles.css
├── internal/
│   ├── auth/
│   │   ├── auth.go
│   │   ├── cli.go
│   │   ├── http.go
│   │   ├── http_test.go
│   │   └── templates/login.html
│   ├── management/
│   │   ├── management.go
│   │   ├── management_test.go
│   │   └── templates/
│   │       ├── index.html
│   │       └── settings.html
│   ├── customer/
│   │   ├── customer.go
│   │   ├── customer_test.go
│   │   └── templates/
│   │       ├── index.html
│   │       └── not-found.html
│   ├── links/
│   │   ├── links.go
│   │   └── links_test.go
│   ├── settings/
│   │   ├── settings.go
│   │   └── settings_test.go
│   └── testdb/testdb.go
├── migrations/.gitkeep
├── compose.yaml
├── go.mod
├── go.sum
├── .air.toml
├── .env.example
├── .gitignore
└── README.md
```

- `main.go`: configuration, GORM initialization and migration, bounded connection pool, route wiring, HTTP timeouts, and native cross-origin request protection.
- `internal/auth/`: staff credentials/CLI, embedded login form, sign-out, account-version authorization exposing the verified username (`auth.Staff`), and SCS table model.
- `internal/management/`: registry and settings handlers and embedded templates; `main.go` injects `auth.Staff` so every write records its account.
- `internal/customer/`: issued-link validation on entry, autosave, and submission; name validation; the submit lock; and embedded customer form and invalid-link templates.
- `internal/links/`: random issuance, digest-only storage, expiry, customer progress status, revocation, GORM model and migration, and PostgreSQL regressions.
- `internal/settings/`: the single settings row, its link-lifetime range (enforced by the store and by database checks), and who last changed it.
- `static/styles.css`: shared interface styles and scoped management/customer layout, embedded and served by the Go application.
- `static/management.js`: management-only native clipboard copying, local-time display of row times, and request-failure feedback; creation and cancellation use declarative HTMX forms.
- `compose.yaml`: local PostgreSQL service and persistent development volume.
- `.env.example`: local database connection template. Air loads the private `.env` copy.
- `internal/testdb/`: integration-only isolated PostgreSQL schemas/pools, including SCS's SQL access.
- `migrations/`: reserved for future versioned GORM migrations. This slice uses `AutoMigrate`; its `.gitkeep` only preserves the directory in version control.

Both interfaces use the same link store. Dependencies are passed explicitly; there is no global database or `init()` setup. Folder separation is not an authorization boundary.

## Cryptographic link design

Each token uses 32 bytes from Go's `crypto/rand`: 256 bits of OS-backed randomness, encoded as 43 URL-safe Base64 characters without padding. Tokens contain no customer data, timestamps, or sequential IDs and are not JWTs.

PostgreSQL stores the SHA-256 digest as a 32-byte binary primary key, the latest saved name, creation, first-visit, submission, cancellation, and expiry timestamps, and the creating and canceling staff usernames. The usable token is never stored in any form, so a database leak yields no working customer URL and staff can't redisplay one. Issuance returns a usable URL only after persistence succeeds.

SHA-256 is the lookup digest, not the source of randomness. Go 1.26's `crypto/rand.Read` terminates the process if the OS random source fails; there is no weaker fallback.

Customer entry, autosave, and submission hash the supplied token and check for a matching database record that is neither canceled nor expired, through GORM's structured conditions. Expiry is compared with the database clock (`expires_at > now()`) on every request, so it is a hard cutoff: a customer mid-form whose link lapses gets the 404 page on their next autosave or submit, and keeps what was already saved. GET and POST record `opened_at` on the first usable-link lookup. Multiple issued tokens remain valid until canceled or expired; opening a link does not consume it. A database error does not grant access. Saving a name updates only the matching, usable, unsubmitted link; it cannot insert or revive a canceled or expired link.

`HEAD /customer/{token}` is a read-only usability check: HTTP 200 even after final submission, 404 for unknown, malformed, canceled, or expired tokens, and 500 for lookup failures. It returns no body, keeps the normal customer response headers, and never changes `opened_at`, saved names, or submission state. A later GET still records the first visit. GET-based previewers can therefore mark a link **Opened**; this is not proof of human interaction.

Tokens are bearer credentials: anyone possessing a usable URL can open it and edit its name without staff authentication until the form is submitted. Management routes require a current staff session, while generated-link and customer responses use `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. Login, management writes, and customer submissions retain Go's native cross-origin protection.

### Name form

The form accepts Unicode names, trims surrounding whitespace, and rejects invalid UTF-8 and NUL characters server-side. Form request bodies are limited to 4 KiB of encoded request bytes, not name characters.

Leaving a changed box sends it to `POST /customer/{token}/draft` (HTMX `change` trigger), so a reload or lost connection keeps every box saved so far. Autosave never sets `submitted_at` and replies only with the status line (**Saved.** or **Not saved.** plus the reason), never redrawing fields the customer may be typing in. Drafts mirror the box, including clearing it; a network failure shows **Not saved. Check your connection.** Text typed but never blurred, or blurred while offline, is not kept. Requests from one box are queued so an older draft can't overwrite a newer one.

**Submit** posts to `POST /customer/{token}`, additionally rejects a blank name, and records `submitted_at`. Invalid names return HTTP 422, malformed forms return HTTP 400, oversized bodies return HTTP 413, and database lookup or name-save failures return HTTP 500. Submission errors retain the form with an inline message: HTMX POST requests receive only the form fragment, while ordinary requests receive a complete page.

A successful submission replaces the form with a read-only confirmation and locks it. Reloading shows the same confirmation. Any later autosave or submit, including one from another tab, returns HTTP 409, and HTMX reloads into the read-only page. The database update itself only matches unsubmitted links, so two tabs racing can't change a submitted form either.

### Testing cancellation

Sign in and click **Create & copy link** three times. The clipboard should hold the latest URL. Open all three customer links. On the first, type a name and press **Submit**; on the second, type a name and click outside the box, which shows **Saved.**, then reload: the name is still there. Reload management: no URL is shown again, and the rows show **Finished**, **In progress**, and **Opened**. A second staff account sees the same list and statuses.

Click **Cancel link** on one row. A confirmation dialog must ask whether you are sure you want to cancel that link. Dismiss it first: no cancellation request should be sent, and the row and customer link must remain active. Click again and confirm. That row changes to **Canceled**, keeps its saved name, and loses its cancel button; the other link must remain usable. Opening or submitting the canceled customer URL must return HTTP 404 with the centered logo and Spanish invalid-link message. Check the title **Enlace no válido | Masterminds** and accents in **válido** and **comuníquese**. A canceled HTMX submission refreshes the whole customer page instead of swapping the error into the name form.

To check that cancellation keeps unrelated URLs, create two links without reloading, then cancel the first. It shows **Canceled**, while the second row keeps its URL and **Copy link**, which still copies a working customer URL. A status change on another row, such as its customer opening it, appears after a reload.

With clipboard completion deliberately delayed for the second link, cancel the first link before clipboard work settles. The second link stays usable, and delayed clipboard success or failure must not overwrite **Link canceled.** Any newer management request invalidates earlier clipboard feedback when it starts, even before its HTTP response arrives.

Click **Cancel all links** and confirm to revoke all active links, including those created before the current page was opened. The status becomes **All links canceled.** and each previously usable row shows **Canceled** with its saved name; expired rows keep their status. The whole list refreshes, so any URL still shown disappears. Creating a new link afterward must still copy a working URL with an empty customer name form.

Cancellation never deletes rows or submitted names; remove unwanted rows directly in PostgreSQL. Repeating cancellation is safe and keeps the first canceler. Cancellation is not a lifetime or expiration setting.

### Testing expiry

Open **Settings**, move the slider to 3 days with the mouse or arrow keys, and **Save**: the page shows **Saved.** and who changed it. Create a link: its row says it expires 3 days after creation, while older rows keep their original expiry. To see the cutoff without waiting, set a link's `expires_at` to the past in PostgreSQL while its customer page is open, then edit the name and click outside the box: the page switches to the 404 page, and management shows **Expired** with the name saved before the cutoff.

## Verification

Integration tests use a real, dedicated PostgreSQL test database. Create it once:

```fish
docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test
```

Run the checks:

```fish
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
go test -race -tags=integration ./...
go vet -tags=integration ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Each integration test creates its own PostgreSQL schema/pool and drops only that schema afterward. Tests guard that no stored column contains a usable token, that migration drops the old recoverable-token column and backfills submissions and expiries only once, token boundaries, status progression, that autosave survives reload without finishing the form, that a submitted form is locked, that expired links stop working even mid-form while keeping saved data, name isolation, that cancellation revokes access while keeping submitted data, and that every create/cancel records its account and anonymous ones are refused. Settings checks cover the default, the 2–14 day range in the store and the database, and the single settings row. Authentication checks cover credential failure, rate limiting, token rotation, persistence/expiry, password/revocation/removal invalidation, username recreation, and that sign-out ends only that browser's session on the server. Real management handlers verify authorization/CSRF boundaries, show-once URLs, shared statuses, audit names from each session, that an HTMX single cancel returns only its own row (saved name and first canceler kept, expired links still **Expired**) while unknown IDs swap nothing and database failures return 500 without a row or notice, that ordinary forms and **Cancel all** return the complete list, and that the slider changes only new links.

Customer HEAD regressions use real HTTP connections and direct persisted-row comparisons: fresh HEAD leaves **Not opened**, GET records the first visit, later HEAD preserves timestamps and draft/submitted data, unusable tokens return 404, and lookup failures remain 500. They check body suppression on the wire, customer response headers, and unsupported-method rejection.

These checks do not prove randomness statistically. Cryptographic randomness comes from the standard library's OS-backed generator, not from a finite collision test.

## Agreed first-part requirements (target workflow)

### Management and link lifetime

Management enters a customer's phone number and generates an invoice link. Link lifetime is configured separately on the **management settings page**, so it does not need to be entered for every generation.

Settings persist in PostgreSQL. The lifetime slider runs from 2 to 14 days in one-day steps, defaulting to 7. Each issued link has an explicit expiration timestamp fixed at creation, so later setting changes affect only new links.

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

Provider webhooks may be appropriate for payment confirmation later. That is separate from communication between this application's management and customer areas. No payment provider has been selected or integrated.

## Implementation approach

The current slice implements link issuance, validation, cancellation, and link-scoped name capture with autosave. Subsequent changes should remain small enough for line-by-line review for correctness, security, readability, and maintainability.

Implement complete, runnable slices rather than disconnected handlers or pretend functionality. Add database constraints and focused regression checks for critical contracts as the relevant behavior is implemented, including customer isolation, expiration boundaries, invoice reuse, and concurrent payment attempts.

The broader paper design remains deferred. Invoice contents and amounts, remaining customer form fields, payment-provider integration, customer verification, and the unresolved link-reissue policy must be clarified before implementing those respective behaviors.
