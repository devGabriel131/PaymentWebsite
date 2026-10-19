# PaymentWebsite

A company payment application with separate management and customer interfaces, one shared Go backend, and PostgreSQL persistence through GORM. The target workflow uses customer-specific invoice links; shared customer links are not part of the design.

## Current status

The runnable slice implements CLI-managed staff access, persistent sign-in, encrypted link history, clipboard copying, cancellation, and a customer name form:

- `/` redirects to `/management/`. Signed-out staff see a login form; signed-in staff see the shared link registry. **Settings** and **Customers** remain visual-only buttons.
- Staff accounts are created through the CLI, not a registration page. Passwords use Argon2id; SCS stores sessions in PostgreSQL for a fixed 14 days. Password changes, revocation, and account removal invalidate existing sessions.
- **Create & copy link** appends and copies a new URL. Every staff account sees the same saved list after reloads and server restarts. Rows show saved customer names, **Copy link**, and **Cancel link**; customer links open in a separate tab.
- `POST /management/links` persists the random token's digest and encrypted recovery value, then returns a row fragment for HTMX or the complete list for ordinary form submissions.
- `POST /management/links/{id}/cancel` deletes one record by its management ID. Other links and names remain active. `POST /management/links/cancel` deletes the complete active registry.
- `/customer/{token}` remains public to anyone holding an issued URL. Unknown, modified, or canceled tokens return the branded HTTP 404 page. Its name form saves only against the matching active link.
- Links issued before encrypted history was enabled remain valid and cancelable, but their original URLs cannot be reconstructed from stored hashes. Their rows show an unavailable-URL notice instead of a fabricated link.

Links are not customer- or invoice-bound yet; the name still belongs to an issued link. Customer-link expiration, full customer records, invoices, and payments remain outside this slice. Sign-in expiry does not expire customer links.

**Test/development only:** management is authenticated, but invoice/payment workflows and customer-link expiration are not implemented. Use disposable data, never real customer/payment data.

## Stack and development

- Go 1.26 or newer, using `net/http` and `html/template`; `go.mod` selects the patched Go 1.27.2 toolchain.
- HTMX for server-rendered interactions; mobile-first pages.
- PostgreSQL for persistence, accessed through GORM and its PostgreSQL driver.
- Railway hosts the shared test deployment; see [Railway test deployment](#railway-test-deployment).
- Air is installed as a project-local Go tool.

Start local development from the project root. The copy step preserves an existing `.env`:

```fish
test -f .env; or cp .env.example .env
# Before the first server start, set LINK_ENCRYPTION_KEY in .env as described below.
docker compose up --detach --wait --wait-timeout 90 db
go tool air
```

Before the first server start, generate a key with `openssl rand -base64 32` and save it once as `LINK_ENCRYPTION_KEY` in private `.env`. Keep `SESSION_COOKIE_SECURE=false` for local HTTP. Preserve an existing key: replacing it makes saved URLs unreadable. Air loads these variables and `DATABASE_URL` from `.env`; shell variables take precedence. Local `.env` files are ignored by Git, and `.env.example` deliberately contains no usable encryption key.

The database credentials in `compose.yaml` and `.env.example` are for local development only. PostgreSQL is bound to `127.0.0.1:5432`; its named volume preserves issued links between container restarts. Start the Compose service before Air if it is not already running.

`DATABASE_URL` is required in both server and CLI modes. The server additionally requires the Base64-encoded 32-byte `LINK_ENCRYPTION_KEY`; `SESSION_COOKIE_SECURE` defaults to true when unset. GORM connects and runs `AutoMigrate` for links, users, and the SCS-compatible sessions table on startup. Models define the tables and constraints; there are no versioned migration files.

Restart an existing Air process after changing its environment so it reloads `.env`. Keep one instance running. Without Air, export the required variables before `go run .`; the application never loads `.env` itself or silently falls back to local credentials.

Use one server or Air instance at a time: they share port 8080, and Air instances also share `tmp/main`.

Open <http://127.0.0.1:8080/management/> and sign in using a CLI-created account. **Create & copy link** copies the same-origin URL and appends it; **Copy link** copies any saved URL again after reload. Clipboard access requires JavaScript and HTTPS or localhost. If blocked, URLs remain available for manual copying; failed creation leaves the clipboard unchanged.

Air watches Go, HTML, CSS, and JavaScript files. Its generated defaults do not automatically refresh the browser; refresh manually. Use `go tool air`, not a separately installed global `air` command. The existing `.air.toml` was generated with `go tool air init`.

Both interfaces load the shared `static/styles.css` stylesheet from `/static/styles.css`, with shared brand colors defined on `:root`. Management-only layout rules are scoped under `.management`; customer rules use `.customer` for the name form and `.customer-not-found` for the invalid-link page. Go embeds the stylesheet, the management clipboard script (`static/management.js`), and the existing Masterminds logo (`static/logo.png`) into the binary and serves them under `/static/`, so deployment needs no separate asset directory. CSS and JavaScript edits require a rebuild, which Air handles during development.

No frontend build step is required. Air's generated `tmp/` directory is ignored by Git.

## Staff accounts and sessions

Create the two staff accounts explicitly. For the default local database:

```fish
set -gx DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite?sslmode=disable'
go run . user add gabriel
go run . user add worker
go run . user list
```

Replace example usernames as needed. Usernames are case-insensitive, normalized to lowercase, and accept 1–64 ASCII letters, digits, `.`, `_`, or `-`. Passwords are 12–1024 bytes and are entered twice at a hidden terminal prompt. Password arguments, environment variables, and piped plaintext are not accepted. Account commands need `DATABASE_URL`, but not the encryption key or a running web server.

`user passwd <username>` changes a password and invalidates prior sessions. `user revoke <username>` invalidates sessions without changing the password; `user remove <username>` removes the account, not its links. Recreating a removed username cannot revive old cookies. Duplicate creation never overwrites an existing password.

Both staff have the same access to the active link registry. Sessions contain account identity/version, not link history. Persistent HttpOnly, SameSite=Lax cookies remember sign-in across tabs and browser restarts; ordinary visits do not extend the 14-day deadline. Production cookies are Secure even behind Railway's TLS proxy. Login attempts share a per-process limit of two initial attempts, then one every 12 seconds. There is no registration, email recovery, or logout UI.

## Railway test deployment

The `PaymentWebsite` service in the existing `amused-light` project deploys `devGabriel131/PaymentWebsite`, branch `master`, using Railpack. Railway names this environment `production`; the application is still for testing only.

Service configuration:

- `DATABASE_URL=${{Postgres.DATABASE_URL}}` references the project's PostgreSQL service over Railway's private network. Local `.env` credentials are not uploaded or reused.
- `LINK_ENCRYPTION_KEY` is a separately generated stable secret for this service. Never reuse the local key, replace it during deployments, or commit it. Keep a secure backup.
- `SESSION_COOKIE_SECURE=true` keeps management cookies HTTPS-only.
- `PORT=8080` makes the application listen on all interfaces for Railway routing. Without `PORT`, local startup remains bound to `127.0.0.1:8080`.
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
│   │   └── templates/index.html
│   ├── customer/
│   │   ├── customer.go
│   │   ├── customer_test.go
│   │   └── templates/
│   │       ├── index.html
│   │       └── not-found.html
│   ├── links/
│   │   ├── links.go
│   │   └── links_test.go
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
- `internal/auth/`: staff credentials/CLI, embedded login form, account-version authorization, and SCS table model.
- `internal/management/`: management handlers and embedded templates.
- `internal/customer/`: issued-link validation on entry and submission, name validation, and embedded customer form and invalid-link templates.
- `internal/links/`: random issuance, digest lookup, AES-GCM recovery, shared inventory, cancellation, GORM model, and PostgreSQL regressions.
- `static/styles.css`: shared interface styles and scoped management/customer layout, embedded and served by the Go application.
- `static/management.js`: management-only native clipboard copying and request-failure feedback; creation and cancellation use declarative HTMX forms.
- `compose.yaml`: local PostgreSQL service and persistent development volume.
- `.env.example`: local database connection template. Air loads the private `.env` copy.
- `internal/testdb/`: integration-only isolated PostgreSQL schemas/pools, including SCS's SQL access.
- `migrations/`: reserved for future versioned GORM migrations. This slice uses `AutoMigrate`; its `.gitkeep` only preserves the directory in version control.

Both interfaces use the same link store. Dependencies are passed explicitly; there is no global database or `init()` setup. Folder separation is not an authorization boundary.

## Cryptographic link design

Each token uses 32 bytes from Go's `crypto/rand`: 256 bits of OS-backed randomness, encoded as 43 URL-safe Base64 characters without padding. Tokens contain no customer data, timestamps, or sequential IDs and are not JWTs. The encryption key protects stored URL recovery, not token validity.

PostgreSQL stores the SHA-256 digest as a 32-byte binary primary key, the submitted name, creation timestamp, and an AES-256-GCM-encrypted copy of the token. `cipher.NewGCMWithRandomNonce` manages nonce generation/storage; the digest is authenticated associated data, so moving ciphertext between rows fails. Issuance returns a usable URL only after persistence succeeds. A wrong key or damaged ciphertext fails the management list rather than hiding records or falling back to plaintext.

Pre-upgrade rows have NULL ciphertext. They remain valid through digest lookup and can be canceled by the hex digest management ID, but their original URLs are unrecoverable from the database. Do not regenerate the encryption key to recover these rows: hashing is not reversible.

SHA-256 is the lookup digest, not the source of randomness. Go 1.26's `crypto/rand.Read` terminates the process if the OS random source fails; there is no weaker fallback.

Customer entry and name submission hash the supplied token and check for a matching database record through GORM's structured conditions. Multiple issued tokens remain valid until canceled; opening a link does not consume it. A database error does not grant access. Saving a name updates only the matching, still-issued link; it cannot insert or revive a canceled link.

Tokens are bearer credentials: anyone possessing a usable URL can open it and edit its name without staff authentication. Management routes require a current staff session, while generated-link and customer responses use `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. Login, management writes, and customer submissions retain Go's native cross-origin protection.

### Name form

The form accepts Unicode names, trims surrounding whitespace, and rejects blank names, invalid UTF-8, and NUL characters server-side. Form request bodies are limited to 4 KiB of encoded request bytes, not name characters.

Invalid names return HTTP 422, malformed forms return HTTP 400, oversized bodies return HTTP 413, and database lookup or name-save failures return HTTP 500. These errors retain the form with an inline message: HTMX POST requests receive only the form fragment, while ordinary requests receive a complete page. Successful submissions show confirmation, and reloading the link restores its saved name.

### Testing cancellation

Sign in and click **Create & copy link** twice. The clipboard should hold the latest URL. Reload: both URLs remain unchanged, and **Copy link** can recopy either. A second staff account sees the same list. Open both customer links.

Click **Cancel link** on one row. A confirmation dialog must ask whether you are sure you want to cancel that link. Dismiss it first: no cancellation request should be sent, and the row and customer link must remain active. Click again and confirm. Only that row disappears; the other link and its saved name must remain usable. Opening or submitting the canceled customer URL must return HTTP 404 with the centered logo and Spanish invalid-link message. Check the title **Enlace no válido | Masterminds** and accents in **válido** and **comuníquese**. A canceled HTMX submission refreshes the whole customer page instead of swapping the error into the name form.

With clipboard completion deliberately delayed for the second link, cancel the first link before clipboard work settles. The second link stays usable, and delayed clipboard success or failure must not overwrite **Link canceled.** Any newer management request invalidates earlier clipboard feedback when it starts, even before its HTTP response arrives.

Click **Cancel all links** and confirm to clear the list and revoke all issued links, including those created before the current page was opened. The status becomes **All links canceled.** Creating a new link afterward must still copy a working URL with an empty customer name form.

This development control permanently deletes the current link registry, including submitted names, through GORM. The permission to perform an unfiltered delete is scoped to this one operation; other database operations retain GORM's global-delete guard. Repeating cancellation on an empty registry is safe. Cancellation is not a lifetime or expiration setting.

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

Each integration test creates its own PostgreSQL schema/pool and drops only that schema afterward. Tests guard encrypted URL recovery, corruption/wrong-key rejection, legacy migration, token boundaries, name isolation, cancellation, and GORM's global-delete guard. Authentication checks cover credential failure, rate limiting, token rotation, persistence/expiry, password/revocation/removal invalidation, and username recreation. Real management handlers verify authorization/CSRF boundaries and shared, reloadable lists.

These checks do not prove randomness statistically. Cryptographic randomness comes from the standard library's OS-backed generator, not from a finite collision test.

## Agreed first-part requirements (target workflow)

### Management and link lifetime

Management enters a customer's phone number and generates an invoice link. Link lifetime is configured separately on the **management settings page**, so it does not need to be entered for every generation.

Settings must persist in PostgreSQL. Each issued link has an explicit expiration timestamp. The initial lifetime, allowed duration range, and effect of later setting changes on already-issued links remain undecided.

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

- Check expiration server-side on initial entry, using an absolute expiration instant, not the browser's clock. PostgreSQL expiration fields use `timestamptz`.
- An expired link returns a customer-facing **This link has expired.** dead end with HTTP 410. An unknown link returns HTTP 404.
- Expiration must not interrupt a customer already moving from form to form.
- Reloading or re-entering through an expired link shows the expired-link page; automatic renewal or recovery is not required for this scope. The customer can contact the company.
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

The current slice implements link issuance, validation, cancellation, and link-scoped name capture. Subsequent changes should remain small enough for line-by-line review for correctness, security, readability, and maintainability.

Implement complete, runnable slices rather than disconnected handlers or pretend functionality. Add database constraints and focused regression checks for critical contracts as the relevant behavior is implemented, including customer isolation, expiration boundaries, invoice reuse, and concurrent payment attempts.

The broader paper design remains deferred. Invoice contents and amounts, remaining customer form fields, payment-provider integration, customer verification, and the unresolved customer-link lifetime and reissue policies must be clarified before implementing those respective behaviors.
