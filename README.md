# PaymentWebsite

A company payment application with separate management and customer interfaces, one shared Go backend, and PostgreSQL persistence through GORM. The target workflow uses customer-specific invoice links; shared customer links are not part of the design.

## Current status

The first runnable slice implements cryptographic link issuance, validation, and bulk cancellation:

- `/` redirects to `/management/`.
- `/management/` has bare HTMX **Generate link** and **Cancel all links** buttons.
- `POST /management/links` creates a random token, persists its hash, and returns a same-origin customer link.
- `POST /management/links/cancel` deletes all issued token hashes. Previously issued customer URLs then return HTTP 404.
- `/customer/{token}` displays **Hello, world!** only for an issued token. Unknown or modified tokens return HTTP 404.
- Issued links survive application restarts and remain independently usable until canceled.

Links are not customer- or invoice-bound yet. Lifetime settings, expiration, customer forms, authentication, invoices, and payments remain outside this slice.

**Development only:** management is not authenticated, and issued links do not expire yet. The server binds to loopback. Do not expose this application publicly or use it for real customer/payment data in this state.

## Stack and development

- Go 1.26 or newer, using `net/http` and `html/template`.
- HTMX for server-rendered interactions; mobile-first pages.
- PostgreSQL for persistence, accessed through GORM and its PostgreSQL driver.
- Railway is the intended deployment platform; deployment is not configured yet.
- Air is installed as a project-local Go tool.

Start local development from the project root. The copy step preserves an existing `.env`:

```fish
test -f .env; or cp .env.example .env
docker compose up --detach --wait --wait-timeout 90 db
go tool air
```

Air loads `DATABASE_URL` automatically from `.env`; no manual Fish environment command is needed. Existing shell environment variables take precedence over `.env`. Local `.env` files are ignored by Git; `.env.example` contains only the local development defaults.

The database credentials in `compose.yaml` and `.env.example` are for local development only. PostgreSQL is bound to `127.0.0.1:5432`; its named volume preserves issued links between container restarts. Start the Compose service before Air if it is not already running.

`DATABASE_URL` is required. GORM connects and runs `AutoMigrate` for the link model on startup. Models define the table and constraints; there are no hand-written SQL queries or migration files.

Restart existing Air processes after changing its configuration so they load `.env`. Keep only one instance running. For a server without Air, export `DATABASE_URL` explicitly before `go run .`; the application itself deliberately requires an environment variable rather than falling back to local credentials.

Use one server or Air instance at a time: they share port 8080, and Air instances also share `tmp/main`.

Open <http://127.0.0.1:8080/management/>. Generated links are same-origin paths; following or copying the link address resolves the full customer URL.

Air watches Go and HTML files. Its generated defaults do not automatically refresh the browser; refresh manually. Use `go tool air`, not a separately installed global `air` command. The existing `.air.toml` was generated with `go tool air init`.

No frontend build step is required. Air's generated `tmp/` directory is ignored by Git.

## Project structure

```text
.
├── main.go
├── internal/
│   ├── management/
│   │   ├── management.go
│   │   └── templates/
│   │       └── index.html
│   ├── customer/
│   │   ├── customer.go
│   │   └── templates/
│   │       └── index.html
│   └── links/
│       ├── links.go
│       └── links_test.go
├── migrations/
│   └── .gitkeep
├── compose.yaml
├── go.mod
├── go.sum
├── .air.toml
├── .env.example
├── .gitignore
└── README.md
```

- `main.go`: configuration, GORM initialization and migration, bounded connection pool, route wiring, HTTP timeouts, and native cross-origin request protection.
- `internal/management/`: management handlers and embedded templates.
- `internal/customer/`: issued-link validation at entry and the embedded customer template.
- `internal/links/`: random token issuance, SHA-256 lookup, GORM model, and PostgreSQL integration tests.
- `compose.yaml`: local PostgreSQL service and persistent development volume.
- `.env.example`: local database connection template. Air loads the private `.env` copy.
- `migrations/`: reserved for future versioned GORM migrations. This slice uses `AutoMigrate`; its `.gitkeep` only preserves the directory in version control.

Both interfaces use the same link store. Dependencies are passed explicitly; there is no global database or `init()` setup. Folder separation is not an authorization boundary.

## Cryptographic link design

Each token uses 32 bytes from Go's `crypto/rand`: 256 bits of OS-backed cryptographic randomness, encoded as 43 URL-safe Base64 characters without padding. Tokens contain no customer data, timestamps, or sequential IDs. No encryption or signing key is needed for this opaque-token design.

PostgreSQL stores only the SHA-256 digest of the encoded token as a binary primary key. The GORM model also defines a 32-byte hash constraint. Issuance returns the usable token only after persistence succeeds; a duplicate hash or failed write returns an error, not a link.

SHA-256 is the lookup digest, not the source of randomness. Go 1.26's `crypto/rand.Read` terminates the process if the OS random source fails; there is no weaker fallback.

Customer entry hashes the supplied token and checks for a matching database record through GORM's structured conditions. Multiple issued tokens remain valid until canceled; opening a link does not consume it. A database error does not grant access.

Tokens are bearer credentials: anyone possessing a usable link can open it. Responses containing generated links and customer entry responses use `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. Management creation and cancellation use Go's native cross-origin request protection, but still need authentication before public deployment.

### Testing cancellation

Open a generated customer link in another tab. On management, click **Cancel all links** and confirm. The generated URL is replaced with **All links canceled.** Reloading an old customer URL must show HTTP 404. Generating a new link afterward must still display Hello World.

This development control permanently deletes the current link registry through GORM. The permission to perform an unfiltered delete is scoped to this one operation; other database operations retain GORM's global-delete guard. Repeating cancellation on an empty registry is safe. Cancellation is not a lifetime or expiration setting.

## Verification

Integration tests use a real, dedicated PostgreSQL test database. Create it once:

```fish
docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test
```

Run the checks:

```fish
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
go test -race -tags=integration ./...
go vet ./...
```

Tests roll back their changes. They guard hash-only persistence, multiple usable links, rejection of unissued or tampered tokens and SQL-like input, and failure to issue a token when persistence fails. Cancellation tests cover rejection of all previous tokens, safe repeated cancellation, successful new issuance, and preservation of the shared database's global-delete guard.

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

Management routes require authorization before being exposed. Authentication and customer-verification mechanisms have not been selected yet.

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

The current slice implements link issuance and validation only. Subsequent changes should remain small enough for line-by-line review for correctness, security, readability, and maintainability.

Implement complete, runnable slices rather than disconnected handlers or pretend functionality. Add database constraints and focused regression checks for critical contracts as the relevant behavior is implemented, including customer isolation, expiration boundaries, invoice reuse, and concurrent payment attempts.

The broader paper design remains deferred. Invoice contents and amounts, customer form fields, payment-provider integration, authentication, and the unresolved lifetime and reissue policies must be clarified before implementing those respective behaviors.
