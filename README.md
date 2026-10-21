# PaymentWebsite

A company payment application with separate management and customer interfaces, one shared Go backend, and PostgreSQL persistence through GORM. The target workflow uses customer-specific invoice links; shared customer links are not part of the design.

## Current status

The runnable slice implements CLI-managed staff access, persistent sign-in, hash-only customer links with progress status and configurable initial expiry, clipboard copying, unfinished-link cancellation, an autosaving customer name form, and a retained, editable Students archive:

- `/` redirects to `/management/`. Signed-out staff see a login form; signed-in staff see a dark workspace with **Customer links**, **Students**, and **Settings** at the top of the desktop sidebar, **Trash** anchored at the bottom-left even while scrolling, their username, and **Sign out**. The shared link registry is horizontally centered in the viewport, with status badges and an empty state. At widths of 960px and below, all navigation moves above the content and wraps normally.
- **Settings** (`/management/settings`) has a slider for initial access to new, unfinished customer forms: 2–14 days in one-day steps, 7 by default. Saving records who changed it. Existing unfinished links keep their issued expiry when the slider changes. Completing a form replaces that deadline with exactly seven days of success-page access from completion.
- Staff accounts are created through the CLI, not a registration page. Passwords use Argon2id; SCS stores sessions in PostgreSQL for a fixed 14 days. Password changes, revocation, and account removal invalidate existing sessions.
- **Create & copy link** appends and copies a new URL. The URL is shown only in that creating response: PostgreSQL stores just its digest, so it can't be recovered later. A lost URL is replaced by creating a new link.
- Every staff account sees the same saved links after reloads and server restarts. **Customer links** shows **Not opened**, **Opened** (first visit recorded), **In progress** (autosaved data, not yet submitted), and **Finished** (submission committed with its student record). Finished links leave this page at 24 hours after completion, on the next load or relevant render; they never enter Trash. **Trash** (`/management/trash`) shows only **Canceled** and **Expired** unfinished links, retaining saved names and audit history without creation or cancellation controls. There is no timer, polling, or deletion job. Rows show creation, completion, and access-expiry times in the viewer's time zone.
- **Students** (`/management/students`) immediately lists every completed submission, newest first, with its editable canonical **Name** and **Submitted** time. Each completion creates one record tied to its source link, not a deduplicated person: submitting the same name through two links creates two records. Students remains available after the source link leaves the main page or its success URL expires. Link and student history are deliberately unpaginated for two staff and small mock data; add pagination before sustained growth.
- Open a student through its name link or row to see centered native-dialog details; the pencil edits one field, **Save** persists it and updates the table row, and **Cancel** reloads the saved field. Editing the student's canonical name never rewrites `Link.Name`, the original submission snapshot, or unlocks the customer's form. Real links and ordinary forms provide full detail/edit pages without JavaScript.
- `POST /management/links` persists the random token's digest, then returns a row fragment for HTMX or the filtered main-page list for ordinary form submissions.
- `POST /management/links/{id}/cancel` revokes one usable, unfinished link by its management ID; `POST /management/links/cancel` revokes every usable, unfinished link matched when its database update runs, so links created afterward, from any tab or account, stay active. Revocation moves the row to **Trash** as **Canceled**, keeping its draft and audit history; there is no deletion UI. Finished links cannot be canceled by either endpoint, even from a stale page or direct request. Expired unfinished links remain **Expired**.
- An HTMX single cancel changes only its addressed row, removing it if canceled, expired, or already archived by the 24-hour rule. Other rows are untouched, so a newly created row keeps its URL and **Copy link**; their statuses refresh on the next reload. An unknown or malformed ID answers HTTP 204, so nothing is swapped, and a failed cancellation answers HTTP 500 without a row or success notice. Ordinary form submissions, **Cancel all unfinished links**, reloads, and navigation render the relevant filtered list, dropping any one-time URLs still on screen.
- While a create or cancel request is in flight, the page disables its buttons, so the normal UI sends one change at a time. This is not request serialization: other tabs, other staff, and direct requests can still interleave, and each is applied on its own.
- Each row is an audit trail: "created … by gabriel · canceled by worker". The username comes from the verified server-side session, never from form input, and the store refuses creation or cancellation without one. **Cancel all unfinished links** records its canceler only on usable, unfinished links it cancels.
- `/customer/{token}` remains public to anyone holding an issued URL. Unknown, modified, canceled, and expired tokens all return the same branded HTTP 404 page. The name form autosaves when the customer leaves a changed box. **Submit** atomically commits the final name, completion timestamp, seven-day access deadline, and student record before showing **Thank you / Your information has been saved.** Completed GETs and repeated final POSTs return the same HTTP 200 success page without changing answers or deadlines; stale draft POSTs return HTTP 409. At the seven-day deadline, even a completed URL returns 404; its student record stays retained.

Links are not invoice-bound or matched to a unique person yet. Students holds the canonical name for each completed submission; broader customer identity/phone records, invoices, and payments remain outside this slice. Sign-in expiry does not expire customer links.

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

The Go binary requires `DATABASE_URL` in both server and CLI modes; the Railway launcher uses the deployed service's environment, not local credentials. `SESSION_COOKIE_SECURE` defaults to true when unset. Startup runs one `AutoMigrate(&auth.User{}, &auth.Session{}, &settings.Settings{}, &links.Link{}, &students.Student{})` over the current models and their constraints. There are no versioned migration files, legacy backfills, or automatic destructive resets. If an obsolete local mock schema cannot migrate, explicitly reset only its disposable `students`/`links` tables as a one-off development operation; never reset live/shared data without separate authorization.

`main.go` owns the shared SQL pool and GORM configuration: maximum open and idle connections are both 10, and `SkipDefaultTransaction: true` avoids GORM's per-write transaction wrapper. PostgreSQL statement atomicity remains intact, including atomic completion/enrollment in one SQL command. Genuine multi-statement units still require explicit transactions. Writes are synchronous and acknowledged only after database success; there is no application cache, write queue, or background draft worker.

Restart an existing Air process after changing its environment so it reloads `.env`. Keep one instance running. Without Air, export the required variables before `go run .`; the application never loads `.env` itself or silently falls back to local credentials.

Use one server or Air instance at a time: they share port 3000, and Air instances also share `tmp/main`. Keep the Air dev server running and reuse it (see `AGENTS.md`).

Open <http://127.0.0.1:3000/management/> and sign in using a CLI-created account. **Create & copy link** copies the same-origin URL and appends it; **Copy link** on that new row recopies it until the page reloads or **Cancel all unfinished links** refreshes the list. Clipboard access requires JavaScript and HTTPS or localhost. If blocked, the new URL remains visible for manual copying; failed creation leaves the clipboard unchanged.

Air watches Go, HTML, CSS, and JavaScript files. Its generated defaults do not automatically refresh the browser; refresh manually. Use `go tool air`, not a separately installed global `air` command. The existing `.air.toml` was generated with `go tool air init`.

On interrupt or SIGTERM, the server stops accepting new connections and allows in-flight handlers up to ten seconds to finish. A failed graceful shutdown force-closes remaining connections; auth session cleanup stops before the SQL pool closes. Active request contexts are not canceled just because shutdown starts. Air's forceful development rebuilds are not this production drain guarantee.

Both interfaces load the shared `static/styles.css` stylesheet from `/static/styles.css`. Customer brand colors are defined on `:root`; management has a scoped dark palette and native dark controls, including sign-in and settings, regardless of the system color preference. Customer rules use `.customer` for the name form and `.customer-not-found` for the invalid-link page. Go embeds the `static/` directory into the binary and serves its files under `/static/`, with the favicon also available at `/favicon.ico`, so deployment needs no separate asset directory. Embedded asset edits require a rebuild; Air handles CSS and JavaScript changes during development.

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

`auth.New(db, secureCookies)` owns SCS construction, cookie settings, and periodic expired-session cleanup. The `management_session` cookie is scoped to `/management/`; SCS wraps login/logout and protected management handlers only. The root redirect, customer routes, static files, and favicon bypass staff-session loading, even when sent a forged staff cookie. Protected requests still verify the current account version against PostgreSQL. `Manager.Close()` stops session cleanup, not the shared SQL pool.

**Sign out** posts to `POST /management/logout`, which deletes that browser's session row and expires its cookie; the account's other browsers stay signed in. To end every session for an account, use `user revoke`.

## Railway test deployment

The `PaymentWebsite` service in the existing `amused-light` project deploys `devGabriel131/PaymentWebsite`, branch `master`, using Railpack. Railway names this environment `production`; the application is still for testing only.

Service configuration:

- `DATABASE_URL=${{Postgres.DATABASE_URL}}` references the project's PostgreSQL service over Railway's private network. Local `.env` credentials are not uploaded or reused.
- `SESSION_COOKIE_SECURE=true` keeps management cookies HTTPS-only.
- `PORT=8080` makes the application listen on all interfaces for Railway routing. Without `PORT`, local startup is bound to `127.0.0.1:3000`.
- The health check remains `/management/`: signed-out requests receive HTTP 200 with only the login form. PostgreSQL connection and migrations must succeed before HTTP starts.
- Railpack uses the preferred Go toolchain declared in `go.mod`; the deployed process does not run Air. Pushes to `master` trigger automatic deployments.

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
│   ├── favicon.ico
│   ├── logo.png
│   ├── management.js
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
│   ├── students/
│   │   ├── students.go
│   │   ├── students_test.go
│   │   ├── fields.go
│   │   └── fields_test.go
│   └── testdb/testdb.go
├── compose.yaml
├── go.mod
├── go.sum
├── .air.toml
├── .env.example
├── .gitignore
└── README.md
```

- `main.go`: configuration, one current-model `AutoMigrate`, shared bounded SQL pool, route wiring, HTTP timeouts, native cross-origin request protection, and signal-driven graceful shutdown.
- `user.fish`: local Railway SSH launcher for deployed staff account commands, preserving interactive password prompts without local database credentials.
- `internal/auth/`: staff credentials/CLI, embedded login form, sign-out, account-version authorization exposing the verified username (`auth.Staff`), and ownership of SCS setup, scoped middleware, and session cleanup.
- `internal/management/`: shared link registry, settings, and authenticated Students routes/templates; `students.go` builds field views and handles detail/edit fragments, while the single `students.html` template owns list/detail pages and shared field/table-row rendering. `main.go` injects `auth.Staff` for link/settings audit attribution.
- `internal/customer/`: public GET/HEAD handling, parse-first autosave/final submission, authoritative save-result dispatch, completion replay/privacy, and embedded customer-content/success and invalid-link templates; name parsing is shared with Students.
- `internal/links/`: random issuance, digest-only storage, initial/completed access expiry, progress status, unfinished-only revocation, authoritative write outcomes, atomic single-command finalization/enrollment, and current GORM model.
- `internal/settings/`: the single settings row, its link-lifetime range (enforced by the store and by database checks), and who last changed it.
- `internal/students/`: retained per-completion model/store, concrete field metadata registry and shared name validation, and field-update regressions. Reads do not filter by source-link age or expiry; updates change only one allowlisted column, never the link snapshot.
- `static/styles.css`: shared interface styles and scoped management/customer layout, embedded and served by the Go application.
- `static/management.js`: link-page-only native clipboard copying and request-failure/clipboard sequencing; creation and cancellation use declarative HTMX forms.
- `static/management-time.js`: shared local-time formatting for link/Trash rows and Students/detail pages, including HTMX-swapped content.
- `static/students.js`: student row activation, native dialog and editor focus, and scoped HTMX validation/network-failure handling; fetching, saving, and canceling remain declarative HTMX interactions.
- `compose.yaml`: local PostgreSQL service and persistent development volume.
- `.env.example`: local database connection template. Air loads the private `.env` copy.
- `internal/testdb/`: integration-only isolated PostgreSQL schemas, one schema-scoped SQL pool per test shared by GORM and SCS.

Both public and management link flows use the same link store; management also receives the student store backed by the same database. Enrollment belongs to the link store's atomic SQL command, not a separate student-store transaction. Dependencies are passed explicitly; there is no global database or `init()` setup. Folder separation is not an authorization boundary.

## Cryptographic link design

Each token uses 32 bytes from Go's `crypto/rand`: 256 bits of OS-backed randomness, encoded as 43 URL-safe Base64 characters without padding. Tokens contain no customer data, timestamps, or sequential IDs and are not JWTs.

PostgreSQL stores the SHA-256 digest as a 32-byte binary primary key, the draft/final submission name, creation, first-visit, submission, cancellation, and expiry timestamps, and the creating and canceling staff usernames. Completion also inserts a student with a unique source-link digest, canonical name, and completion time; subsequent staff edits change only the student name. The usable token is never stored in any form, so a database leak yields no working customer URL and staff can't redisplay one. Issuance returns a usable URL only after persistence succeeds.

SHA-256 is the lookup digest, not the source of randomness. Go 1.26's `crypto/rand.Read` terminates the process if the OS random source fails; there is no weaker fallback.

Customer reads and guarded writes hash the supplied token and check for a matching database record that is neither canceled nor expired, using bound SQL parameters. Expiry is compared with the database clock (`expires_at > now()`) on each database operation, so it is a hard cutoff: a customer mid-form whose link lapses gets the 404 page on their next autosave or submit, and keeps what was already saved. Only GET records `opened_at`; HEAD is read-only, and POST never records a visit. Repeat GETs of an already-opened link are SELECT-only. The first GET conditionally stamps and returns the first-visit row; if a concurrent open, cancellation, or expiry prevents that update, the store rereads usability rather than returning stale access. Multiple issued tokens remain valid until canceled or expired; opening a link does not consume it. A database error does not grant access. Saving a name updates only the matching, usable, unsubmitted link; it cannot insert or revive a canceled or expired link.

`HEAD /customer/{token}` is a read-only usability check: HTTP 200 even after final submission, 404 for unknown, malformed, canceled, or expired tokens, and 500 for lookup failures. It returns no body, keeps the normal customer response headers, and never changes `opened_at`, saved names, or submission state. A later GET still records the first visit. GET-based previewers can therefore mark a link **Opened**; this is not proof of human interaction.

Tokens are bearer credentials: anyone possessing a usable URL can open it and edit its name without staff authentication until the form is submitted. Management routes require a current staff session, while generated-link and customer responses use `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. Login, management writes, and customer submissions retain Go's native cross-origin protection.

GORM's parameterized SQL logs omit bound values. This is not a blanket PII-redaction guarantee: database error messages can themselves contain submitted values. Student field-save failures suppress SQL logging and report only record/field metadata and the error type.

### Name form

The form accepts Unicode names, trims surrounding whitespace, and rejects invalid UTF-8 and NUL characters server-side through the same typed name parser used by Students. Form request bodies are limited to 4 KiB of encoded request bytes, not name characters.

Leaving a changed box sends it to `POST /customer/{token}/draft` (HTMX `change` trigger). A successful draft persists across reloads without setting `submitted_at` or creating a student, and replies only with the status line (**Saved.** or **Not saved.** plus the reason), never redrawing the input. Drafts mirror the box, including clearing it; a network failure shows **Not saved. Check your connection.** Text typed but never blurred, or blurred while offline, is not kept. Requests from the box queue the latest pending change so an older draft cannot overwrite a newer one.

**Submit** posts to `POST /customer/{token}` and additionally rejects a blank name. Both customer POST handlers cap, parse, and validate the body before database access. Valid input calls `SaveDraft` or `Submit` directly, without an HTTP-layer preflight or post-save lookup. The store returns an authoritative `links.SaveResult`: `Saved` means this operation committed; `AlreadySubmitted` means the still-usable link was already finalized; `Unavailable` (zero) means malformed, unknown, canceled, or expired. Database errors return `Unavailable` plus an error, never a successful outcome.

`Saved` returns draft status-only HTTP 200 or final confirmation HTTP 200. `AlreadySubmitted` returns final confirmation HTTP 200 or stale-draft HTTP 409. `Unavailable` returns 404; database failures return a retryable 500 without claiming success. For malformed, oversized, or invalid input, a read-only lookup first resolves link state: unusable links still return 404, and usable completed links still return final 200 or draft 409, even for malformed/oversized bodies. Only usable unfinished links receive parse/validation errors: 400 for malformed forms, 413 for oversized bodies, and 422 for invalid names. Errors retain the saved draft name on 400/413 and the attempted name on validation/save failure.

HTMX final POSTs return `customer-response`, containing the stable `#customer-content` section; the form swaps that whole section, including its heading, while drafts still target only `#save-status`. Ordinary requests receive a complete page. Responses retain `Vary: HX-Request`; HTMX 404/409 responses request a full refresh.

A successful submission uses one PostgreSQL data-modifying CTE command: a guarded link UPDATE returns the final name/digest/timestamp directly into the student INSERT. It atomically commits the final name, `submitted_at`, exactly 168 hours of success-page access, and one student record. Insert or deferred commit failure rolls all changes back, preserving the draft and initial expiry for retry. Only after database success does the form disappear, replaced by **Thank you / Your information has been saved.** and the title **Submission complete | Masterminds**.

Reloads and repeated final POSTs return HTTP 200 success until the completion deadline. Replays may parse replacement input in memory, but never save it, expose submitted/canonical names, create another student, or move completion/access deadlines. A stale draft, including one from another tab, returns HTTP 409 with `HX-Refresh: true` for HTMX, reloading into success without claiming its text was saved. Conditional database writes prevent racing drafts or final submissions from changing a completed form. If cancellation wins before submission, no student is created; if submission wins, cancellation cannot revoke it. On a no-op write only, the link store rereads active submission state to distinguish `AlreadySubmitted` from `Unavailable`.

### Students details and field editing

The Students table shows **Name** and **Submitted**, ordered newest completion first. Its name anchor supports Tab/Enter and ordinary navigation; pointer clicks elsewhere on the row activate it without replacing table semantics or interfering with selected text or other controls. With HTMX and JavaScript, details load into a centered native `<dialog>` with Close, Escape, and native focus containment. Closing resolves the initiating row anchor again and restores focus even after a save replaced that row. Without JavaScript, the same anchor opens a full detail page with **Back to students**, and pencil links, Save forms, and Cancel links still work.

Each pencil opens only its addressed field. Save updates exactly one registered column using a bound value; an HTMX modal save returns the field plus an out-of-band table row, while standalone saves redirect to the full detail page. Validation returns HTTP 422 with the attempted value and accessible inline error, ready for correction; unknown fields or missing students return 404, malformed/oversized bodies return 400/413, and database failures return 500 rather than a saved notice. Field requests queue within the details panel. Other HTTP or network failures retain the current table, saved values, and unsaved input with a nearby retry message; expired staff sessions redirect to login instead of injecting login HTML into a field.

Fields come from the ordered, package-owned `students.Field` metadata registry, not an interface, reflection, schema discovery, JSON/EAV storage, or arbitrary-column writes. `Field` is a concrete struct with `Key`, `Label`, `InputType`, `Required`, and `Value`/`Parse` functions; callers treat the registry as read-only. To add a field, add a typed `Student` column and a metadata entry defining its presentation, value, and parser. Table headings/cells and detail/editor templates already iterate the registry. Customer collection for additional fields is a separate change; only Name exists today.

### Testing cancellation

Sign in and click **Create & copy link** three times. The clipboard should hold the latest URL. Open all three customer links. On the first, type a name and press **Submit**: check the success heading/title and that **Students** immediately contains exactly one record. On the second, type a name and click outside the box, which shows **Saved.**, then reload: the draft is still there without a student. Reload management: no URL is shown again, and the rows show **Finished**, **In progress**, and **Opened**. The finished row has no cancel button; a second staff account sees the same shared list and student.

Click **Cancel link** on the unfinished **In progress** row. A confirmation dialog must ask whether you are sure you want to cancel that link. Dismiss it first: no cancellation request should be sent, and the row and customer link must remain active. Click again and confirm. That row disappears from the main page and appears in **Trash** as **Canceled**, keeping its saved draft and audit history without a cancel button; the other unfinished link and the finished success page remain usable. Opening or submitting the canceled customer URL must return HTTP 404 with the centered logo and Spanish invalid-link message. Check the title **Enlace no válido | Masterminds** and accents in **válido** and **comuníquese**. A canceled HTMX submission refreshes the whole customer page instead of swapping the error into the name form.

To check that cancellation keeps unrelated URLs, create two links without reloading, then cancel the first. It disappears from the main page, while the second row keeps its URL and **Copy link**, which still copies a working customer URL. A status change on another row, such as its customer opening it, appears after a reload.

With clipboard completion deliberately delayed for the second link, cancel the first link before clipboard work settles. The second link stays usable, and delayed clipboard success or failure must not overwrite **Link canceled.** Any newer management request invalidates earlier clipboard feedback when it starts, even before its HTTP response arrives.

Click **Cancel all unfinished links** and confirm to revoke all usable unfinished links, including those created before the current page was opened. The status becomes **All unfinished links canceled.** and those rows move to **Trash** as **Canceled**, keeping saved drafts; expired rows keep their status. Finished links, their success pages, student records, and cancellation audit remain unchanged. The main list refreshes, so any URL still shown disappears. Creating a new link afterward must still copy a working URL with an empty customer name form.

Cancellation never deletes rows or saved drafts; Trash retains them. Repeating cancellation is safe and keeps the first canceler. Cancellation is not a lifetime or expiration setting.

### Testing expiry

Open **Settings**, move the slider to 3 days with the mouse or arrow keys, and **Save**: the page shows **Saved.** and who changed it. Create a link: its row says it expires 3 days after creation, while older unfinished rows keep their original expiry. Submit one such link: the finished row must instead show success-page expiry seven days after completion, regardless of the slider.

For disposable local data only, set an unfinished link's `expires_at` to the past in PostgreSQL while its customer page is open, then edit the name and click outside the box: the page switches to the 404 page. Reload management: the link is absent from **Customer links** and appears in **Trash** as **Expired**, with the name saved before the cutoff and no student.

To check retention without waiting, set a completed link's `submitted_at` to 25 hours ago and its student's `created_at` to the same instant, keeping `expires_at` seven days after that completion. The already-open main page does not poll; reload it and the finished row must disappear without entering Trash. Students must retain it, and the customer URL must still show success. Then set that completed link's `expires_at` to the past: GET/HEAD and final POST return 404, but its student remains available and editable.

### Testing Students

Submit two links with the same name and check that Students shows two separate records. Open one by pointer and again with Tab/Enter; test Close and Escape, including focus returning to the initiating anchor. Use the pencil, submit a whitespace-only name to exercise server-side 422, correct it to a Unicode name, and save: the field and table row update without closing the dialog and remain changed after reload. The other student and original link submission snapshot must remain unchanged. Reopen the pencil, type a different value, then **Cancel**: the saved name returns.

With a field editor open and unsaved text present, block its request or disconnect the browser and try Save/Cancel: a nearby error must appear without losing the input, saved table row, or existing details. Restore the connection and retry. Repeat the edit/save/cancel flow with JavaScript disabled through the full detail page. Check desktop and mobile layouts, table scrolling without whole-page overflow, dialog internal scrolling, and visible keyboard focus. Keep link clipboard/cancel checks separate from Students: its pages must not load link-only clipboard behavior.

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

Each integration test creates its own PostgreSQL schema through one schema-scoped pool shared by GORM and direct SQL/SCS access. Cleanup closes the HTTP server, stops auth session cleanup when present, drops only the generated schema, then closes its pool. Regression contracts include:

- Digest-only storage and token boundaries; status progression, draft persistence without enrollment, independent link/name state, hard access cutoffs, and verified staff attribution for creation/cancellation.
- Source-link foreign-key protection against deletion/hash changes, and repeated current-model `AutoMigrate` preserving links and students.
- Exactly one enrollment under concurrent final submissions; rollback of name/completion/expiry on student insert or deferred commit failure; successful retry; submission/cancellation races with either completion plus one student or cancellation plus none.
- Completed GET/final-POST HTTP 200 replay without answer/deadline changes, including invalid-body replay precedence, stale-draft 409, retryable customer form fragments with retained names, and completed-access expiry without student loss.
- The exact 24-hour completion boundary, next-render main-page archival, no completed records in Trash, stale cancel unable to reinsert an archived row, and Students retention after source expiry.
- Student newest-first reads, typed field validation/allowlisting and escaped rendering, canonical-name edits isolated from the original link snapshot and other students, full-page/HTMX editor and modal/OOB-row contracts, rejected requests without mutation, and honest empty/error responses.
- Real management authentication/CSRF boundaries including Students, show-once URLs, row-local cancellation, first-canceler preservation, missing-ID no-swap behavior, database failures without success fragments, and unfinished-only single/bulk cancellation.
- Settings default, store/database 2–14 day constraints and singleton row, changes affecting only new unfinished access; authentication credential failure, rate limiting, token rotation, persistence/expiry, password/revocation/removal invalidation, username recreation, browser-local sign-out, and public customer access bypassing staff-session storage.

Customer HEAD regressions use real HTTP connections and direct persisted-row comparisons: fresh HEAD leaves **Not opened**, GET records the first visit, later HEAD preserves timestamps and draft/submitted data, unusable tokens return 404, and lookup failures remain 500. They check body suppression on the wire, customer response headers, and unsupported-method rejection. Separate link-store regressions protect the populated first-GET result, unchanged repeat-open timestamp, and cancellation winning while the first-open update waits. POST never records a first visit.

These checks do not prove randomness statistically. Cryptographic randomness comes from the standard library's OS-backed generator, not from a finite collision test.

Browser smoke checks complement those server/database regressions: native dialog/focus behavior after an OOB row replacement, field validation/cancel/retry and intercepted-network input retention, clipboard isolation/sequencing, the next-load archive cutoff, and desktop/mobile layout need a real browser. They are not proved by HTML string assertions alone.

## Agreed first-part requirements (target workflow)

The following describes the future customer/invoice/payment workflow, not additional implemented behavior. The Students archive above is per completion and does not yet supply this workflow's unique customer identity or invoice binding.

### Management and link lifetime

Management enters a customer's phone number and generates an invoice link. Link lifetime is configured separately on the **management settings page**, so it does not need to be entered for every generation.

Settings persist in PostgreSQL. The lifetime slider runs from 2 to 14 days in one-day steps, defaulting to 7. Each issued link gets an explicit initial expiration timestamp; later setting changes affect only new links. In the current form slice, completion replaces initial access with seven days of success-page access. Future invoice/payment transitions must define their own behavior before implementation.

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

The current slice implements link issuance, validation, unfinished-only cancellation, name autosave, atomic completion/enrollment, and the retained Students table/details with canonical-name editing. Subsequent changes should remain small enough for line-by-line review for correctness, security, readability, and maintainability.

Implement complete, runnable slices rather than disconnected handlers or pretend functionality. Add database constraints and focused regression checks for critical contracts as the relevant behavior is implemented, including customer isolation, expiration boundaries, invoice reuse, and concurrent payment attempts.

The broader paper design remains deferred. Invoice contents and amounts, remaining customer form fields, payment-provider integration, customer verification, and the unresolved link-reissue policy must be clarified before implementing those respective behaviors.
