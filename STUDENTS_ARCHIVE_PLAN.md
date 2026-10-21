# Students archive and editable customer records

## Context

Finished links must leave the management main page 24 hours after completion; every completed submission must immediately create a retained record in a new Students table/tab. Students needs a customer-data table and a centered HTMX detail dialog with a pencil beside every editable field, backed by an extensible Go field interface.

Confirmed behavior: apply main-page cutoff on the next page load, not with background polling. Finished links cannot be canceled, including by bulk cancellation; they serve a success page until exactly seven days after completion, then expire. Show success only after both submitted data and the student record commit successfully. Future payment behavior is not part of this change.

## Findings

- `internal/links/links.go`: `Link` currently owns `Name`, draft and final data; no separate customer model in this flow. `SubmittedAt` records final submission; autosave leaves it unset. `Store.Submit` locks subsequent customer writes. `Entry` currently omits `SubmittedAt`.
- `internal/links/links.go`: cancellation wins status precedence over submission; a finished link can currently be canceled. `Store.List` includes every link; canceled/expired records are retained.
- `internal/management/management.go`: main and Trash share `pageData` and filter `Store.List` with `Entry.Trashed`. POST HTMX responses return changed rows, preserving other one-time URLs. Settings uses independent page data.
- `internal/management/templates/index.html`: shared `header` and `navigation`; links, Settings, and Trash. HTMX 2.0.11 is CDN-pinned with SRI.
- `static/management.js`: link-specific DOM assumptions (`#generated-links`, `#link-status`); cannot include unchanged on Students. Contains reusable local-time formatting behavior.
- `internal/customer/customer.go`: final submit and blur draft save share a handler; UTF-8/NUL validation and nonempty final name currently live here. Customer writes are capped at 4 KiB.
- `main.go:73-78,117-134`: startup migrations, authenticated management child mux, and native same-origin protection. New Students routes must remain on that child mux.
- `internal/testdb/testdb.go`: integration tests create isolated PostgreSQL schemas; fixtures usually call `AutoMigrate` directly, not `links.Migrate`.
- No existing modal/table/field-editor primitive exists. Management CSS provides dark palette, 44px controls, native focus outlines, shared panels, and a ≤960px wrapping navigation layout.
- Port 3000 already has a listener. Reuse existing Air-managed app; never start another server.

## Approach

### Phase 1 — Atomic enrollment and terminal completion

Keep existing flat packages. Add `internal/students/students.go` (new leaf package): no student/customer persistence currently exists. Keep `links.Link.Name` as the original submission snapshot; Students owns a separately editable record. Student edits must not rewrite the snapshot or unlock the public form.

#### Schema and cutover

Define:

```go
type Student struct {
    ID             uint64    `gorm:"primaryKey"`
    SourceLinkHash []byte    `gorm:"not null;uniqueIndex;check:student_source_hash_size,octet_length(source_link_hash) = 32"`
    Name           string    `gorm:"not null"`
    CreatedAt      time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}
```

`CreatedAt` is completion/enrollment time. No raw token, soft-delete column, payment fields or name-based deduplication. Add `Student *students.Student` to `links.Link` with `gorm:"foreignKey:SourceLinkHash;references:TokenHash;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`. This has-one association expresses the FK without making `students` import `links`; ordinary link queries need no preload.

Replace `links.Migrate`'s legacy backfills with a transactional cutover:
1. If `students` does not exist, drop the existing mock `links` table if present.
2. `AutoMigrate(&Link{}, &students.Student{})`.
3. Subsequent startups, with `students` present, only AutoMigrate; never clear rows.

This intentionally loses existing mock links/drafts/submissions once, permitted by repository policy. Preserve accounts, sessions and settings. Drop/recreate plus Students creation must commit atomically. Remove old ciphertext/submission/expiry compatibility logic and unused settings import. Keep `main.go` calling `links.Migrate` after auth/settings migration; no external reset command or migration framework.

#### Commit completion and student insertion together

Keep `links.New`, `Store.SaveDraft`, `Store.Submit`, and public customer `RegisterRoutes` signatures unchanged. `Submit(ctx context.Context, token, name string) (bool, error)` becomes:
1. Validate/hash token with existing `tokenDigest`; malformed tokens return false, nil.
2. Begin `s.db.WithContext(ctx).Transaction`. Reuse a transaction-scoped `Store.active`, add `submitted_at IS NULL`, and use `clause.Returning{}` into a `Link` to update `name`, `submitted_at = now()`, and `expires_at = now() + interval '168 hours'`.
3. Zero matched rows → false, nil, no student. One row → insert `students.Student{SourceLinkHash: completed.TokenHash, Name: completed.Name, CreatedAt: *completed.SubmittedAt}` through the same transaction.
4. Return true only after commit succeeds. Any update/insert/commit error returns false plus error, with completion, final name and expiry rolled back.

The conditional update and unique source hash serialize competing submit/draft/cancel actions and prevent duplicate enrollment. Keep draft saving conditional on usable/unsubmitted status; inline the old private `update` helper into `SaveDraft` once Submit no longer uses it. Never use a separately pool-bound student store inside the transaction.

#### Completion access and cancellation

Change `Entry.Cancelable`, `Store.Cancel`, and `Store.CancelAll` to exclude finished links. Both cancellation SQL updates add `submitted_at IS NULL`; hiding buttons alone is insufficient. Canceling a finished link returns its unchanged entry, no token and no cancellation audit. Bulk cancellation revokes only usable unfinished links. Keep missing-ID 204 HTMX behavior and first-canceler preservation.

Submission wins race → cancellation does nothing; cancellation wins → no student. `Finished` stays Finished after success-page expiry and never enters Trash. Add `func (e Entry) AccessExpired() bool` returning `!time.Now().Before(e.ExpiresAt)`; use this for expiry wording instead of `Cancelable`. Update the ExpiresAt comment and `internal/management/templates/settings.html` scope text: settings controls initial unfinished access; completion replaces it with seven days from completion. Bulk button/confirmation/notice explicitly refer to unfinished links; notice `All unfinished links canceled.`.

In `internal/customer/customer.go`:
- Successful final POST → HTTP 200 only after commit; HTMX gets completed-content fragment, ordinary requests get full success page.
- GET of unexpired completed token → full success page.
- Repeated final POST → same 200 success without parsing/writing replacement answers or moving either deadline.
- Stale draft POST → retain 409 and HTMX `HX-Refresh: true`, reloading into success rather than claiming stale answers saved.
- Lost conditional update → re-read: completed/unexpired gets final-POST success or draft conflict above; canceled/expired/missing gets existing 404; failed lookup gets 500.
- Preserve HEAD as read-only 200/404/500 with no body, and existing retryable 400/413/422/500 form errors.

In `internal/customer/templates/index.html`, use a named `customer-content` fragment wrapping heading and form/confirmation in `<section id="customer-content">`. Final form targets that section with outerHTML; draft still targets only `#save-status`. Add `customer-response`, containing a conditional `<title>` followed by customer-content. Change the handler's final-POST HTMX branch from name-form to customer-response for both success and form errors; full index renders its own head and customer-content, avoiding duplicate titles. Success heading: `Thank you`; text: `Your information has been saved.`; title: `Submission complete | Masterminds`. Never leave the old question heading or editable input on screen, and error swaps must retain the section target for retries. Do not expose editable student data through the finished bearer link. Keep existing customer style, invalid-link page and pinned HTMX.

Checkpoint: actual final submit creates one student and shows success; failure remains retryable; cancellation cannot revoke completion; GET/HEAD return 404 after the seven-day expiry. Adapt affected lifecycle tests within this phase.

### Phase 2 — Students archive and 24-hour main-page visibility

Depends on Phase 1. Add a working read-only table here; Phase 3 adds row actions, not placeholder buttons.

#### Stable field interface and student reads

Add `internal/students/fields.go` (new). No existing field-definition API exists. Define a typed field interface, not reflection, schema introspection, an EAV/JSON table or arbitrary-column CRUD:

```go
type Field interface {
    Key() string
    Label() string
    InputType() string
    Required() bool
    Value(Student) string
    Parse(string) (any, error)
}
func Fields() []Field
func FindField(key string) (Field, bool)
func ParseName(raw string, required bool) (string, error)
type ValidationError struct { Message string }
func (e *ValidationError) Error() string
```

Use a package-private ordered registry, exposed read-only by `Fields` contract. Initially one unexported `nameField`: key `name`, label `Name`, input type `text`, required true, value `Student.Name`, Parse delegates to `ParseName(raw, true)`. `ParseName` trims whitespace, rejects invalid UTF-8/NUL with `Enter a valid name.`, and rejects required empty values with `Enter a name.`; drafts pass false. Move equivalent validation out of `internal/customer/customer.go` and reuse this function; retain its 4 KiB encoded request cap. `Parse` returns a typed database value or `*ValidationError`, not database-error text.

Future fields require a typed `Student` column plus field implementation/registry entry; management headings, cells, details and editors iterate that registry unchanged. Future customer collection remains separate; do not invent fields now.

In `students.go`, add:

```go
type Store struct { db *gorm.DB }
func New(db *gorm.DB) *Store
func (s *Store) List(ctx context.Context) ([]Student, error)
func (s *Store) Get(ctx context.Context, id uint64) (*Student, error)
```

List orders `created_at DESC, id DESC`, with no source-link status/expiry filter. Follow existing unbounded small-mock-history convention; paginate before sustained growth. Get returns nil, nil for missing/zero ID; database failures propagate.

#### Management route and table

Change existing registration to:

```go
func RegisterRoutes(mux *http.ServeMux, store *links.Store,
    roster *students.Store, config *settings.Store,
    staff func(context.Context) string) error
```

Add `internal/management/students.go` (new) for student HTTP/view logic. Existing registration calls `registerStudentRoutes(mux *http.ServeMux, roster *students.Store, staff func(context.Context) string, execute func(http.ResponseWriter, int, string, any))`, reusing parsed templates and secure execute closure. Use five-second request contexts. Keep every Students endpoint on the authenticated management child mux, behind existing same-origin protection.

`GET /management/students` renders new `internal/management/templates/students.html`, reusing header/navigation/panels and a semantic table. Add Students between Customer links and Settings in shared navigation, with `aria-current="page"`. Columns: every registry field in order, then `Submitted` from CreatedAt. Build typed display view data from the registry once per student, reused for rows/details/edit results in Phase 3. Do not show IDs/hashes as customer fields. Empty state: `No students yet` / `Completed customer forms will appear here.` Database failure: 500 `Could not load students. Please try again.`, no fake empty table.

#### Main-page archive rule

Add `SubmittedAt *time.Time` to `links.Entry`, populated by `Link.entry`. Add `func (e Entry) Archived(now time.Time) bool`: true only for Finished with non-nil completion at or before `now.Add(-24*time.Hour)`. Capture one now per management render. Main filtering excludes Trashed and Archived; Trash includes only Trashed. `Store.List` stays all-history, oldest-first and token-free.

Apply the same cutoff to single-row HTMX results: add `ShowLink bool` to pageData, compute it from addressed row/current cutoff, and replace `"result"`'s `not .Link.Trashed` condition with `.ShowLink`. A stale cancel cannot reinsert an archived finished row. Preserve unrelated one-time URL DOM nodes. No timer, polling, scheduled job or deletion.

Update main explanation/empty state/footer: finished links stay here 24 hours; their information remains in Students. Finished rows show completion and success-page expiry, not a cancel action.

Checkpoint: new completion immediately appears in both lists; at 24 hours it vanishes from main on reload, remains in Students, never Trash. Students remains after source-link expiry.

### Phase 3 — Centered details and per-field pencil editing

Depends on Phase 2 registry/store/page. Reuse typed validation, html/template, secure rendering and authenticated mux.

#### Safe field mutation

Add `ErrUnknownField = errors.New("students: unknown field")` and:

```go
func (s *Store) UpdateField(ctx context.Context, id uint64,
    key, raw string) (*Student, error)
```

Resolve key with FindField before SQL. Unknown key → ErrUnknownField; invalid parsed value → `*ValidationError`, no write. Update exactly one allowlisted column with a bound value and `WHERE id = ?`; use `clause.Returning{}` to return persisted Student without a second read failing after commit. Zero matched rows → nil, nil. Never use GORM Save or write a whole stale record. Source hash, ID and timestamps are not editable. Different-field writes cannot overwrite each other; same-field concurrent saves use database last-write-wins.

#### Endpoints and HTML contracts

| Authenticated route | Ordinary response | HTMX response |
|---|---|---|
| `GET /management/students/{id}` | Full detail page | Modal content only |
| `GET /management/students/{id}/fields/{field}?edit=1` | Full detail page, addressed field editing | Addressed editor only |
| `GET /management/students/{id}/fields/{field}` | Full detail page, read mode | Addressed read field (Cancel) |
| `POST /management/students/{id}/fields/{field}` | Success 303 to `/management/students/{id}?saved={field}` | Success 200 field plus OOB table row when modal |

Parse IDs using `strconv.ParseUint(..., 10, 64)`; malformed/zero/missing student or unknown field → 404 `Student or field not found.`. Field handlers resolve the registered field and load the student before editing/parsing values, so nonexistent records are not reported as validation failures. GET never mutates. POST caps encoded body at 4 KiB using MaxBytesReader and reads only `value` plus presentation-only `modal`. Malformed form → 400 `Could not read this field. Please try again.`; oversized → 413 `This value is too large.`; validation → 422 editor with attempted value and inline error. Ordinary 422 renders full detail page. Database failure → 500 `Could not save this field. Please try again.` (reads: `Could not load student. Please try again.`), never a success fragment. Log errors server-side, not submitted field content.

Add `internal/management/templates/student.html` (new) with full detail page and named shared detail/field/editor/save fragments. Heading `Student details`; read-only Submitted metadata; one labeled row per registry field. Pencil is a real fallback link, accessible name `Edit {Label}`, decorative SVG. Editor has labeled input, Save and Cancel. Errors use `role="alert"`, aria-invalid and aria-describedby; saved field gets a role=status Saved notice. Each field block has `id="student-field-{id}-{key}"`; each table row `id="student-row-{id}"`. Single-field edits never replace the whole dialog or discard other open editors.

Each field action targets its closest `.student-field` with outerHTML. Wrap fields in stable `.student-details` and inherit `hx-sync="closest .student-details:queue all"` so GET/edit/save/cancel requests serialize across fields; delayed responses cannot overwrite the table with an older snapshot. Forms use hx-post, method=post, real action and hx-disabled-elt for their buttons. Cancel is a GET link, never a form reset masquerading as persisted data. After outerHTML swaps, resolve the new field by its stable ID before focusing input/pencil.

Modal pencil/cancel GET URLs carry `modal=1`; editor POST includes hidden `modal=1`. Standalone detail fields omit it. Presentation flag has no authorization effect. Successful modal save returns changed field plus `<template><tr id="student-row-{id}" hx-swap-oob="outerHTML">…</tr></template>` rendered by shared row template. Standalone saves omit the OOB row, avoiding missing table targets. Reloading table/dialog must show persisted value.

#### Row interaction and native dialog

Name cell contains a real detail anchor with hx-get, hx-target `#student-dialog-content`, innerHTML swap. Pointer clicks elsewhere on a row activate that anchor unless another interactive element was clicked or text is selected. Keyboard users get normal Tab/Enter on anchor; preserve table semantics instead of role=button on tr.

Students page has one persistent `<dialog id="student-dialog" aria-labelledby="student-heading">` and target `#student-dialog-content`. Fetched content contains heading `student-heading`, shared fields, and visible Close button in its own `form method="dialog"`. Call showModal only after a successful content swap. Native Escape/focus containment remain. Close/Escape returns focus to initiating row anchor, resolved again by row ID because save can replace the row. Full detail fallback shares fields and Back to students link, not dialog-only close form.

Add `static/students.js` (new), loaded only on Students/full detail pages. Custom JS handles row activation, native dialog lifecycle/focus, editor focus after swaps, and scoped HTMX failures; all fetching/saving/canceling uses attributes. Scope beforeSwap's 422 opt-in to student field editors; never enable arbitrary 4xx/5xx swapping globally. Other request/network failures retain existing table/values/unsaved inputs and show a nearby accessible error. Honor existing 401 HX-Redirect; never inject login HTML into fields. Gate dialog-only behavior when standalone detail page has no dialog.

Move local-time formatter/onLoad block from `static/management.js` into new `static/management-time.js`; include on registry/Trash and Students/detail. Leave clipboard/error sequencing in management.js unchanged. Do not load link-only JS on Students or duplicate time formatting. Keep pinned HTMX 2.0.11/SRI; no new dependencies/build step.

#### Visual layout

Reuse existing palette/type: canvas `#15232b`, surface `#1c2d36`, border `#3b535e`, text `#edf3f5`, muted `#afc0c7`, accent `#9ccdd0`; JetBrains Mono and current type scale. Left-align table data; border-separated rows, restrained hover/focus, no card decoration.

Add management-scoped table/dialog/editor CSS in `static/styles.css`. Reuse `.management-links`, `.management-panel`, `.management-button-primary` and focus rules. Table wrapper scrolls horizontally as columns grow; names wrap, no whole-page overflow. Dialog: margin auto, width `min(40rem, calc(100vw - 2rem))`, max-height `calc(100dvh - 2rem)`, internal overflow, existing panel radius/border and subdued backdrop. Controls stay ≥44px; editor controls use palette, not login-page selectors. Retain ≤960px wrapping navigation. No animation/new color system.

Checkpoint: row/keyboard opens centered details; pencil → invalid/save/cancel works without page navigation; save updates table and survives reload. Verify mobile scrolling, focus restoration and failure retention in browser.

### Integration across phases

Each phase adapts affected behavior tests and remains runnable. Phase 1 precedes 2; Phase 2 precedes 3. Within Phase 3, store mutation and static CSS/dialog work are independent once endpoint/DOM contracts above are fixed. Do not leave compatibility aliases or tests asserting superseded behavior.

Changed management registration callers verified: `main.go:119`, `internal/management/management_test.go:63,520`, `internal/auth/http_test.go:191`. Pass students.New with same DB as link/settings stores; failed-store fixture passes failed transaction to all stores. LSP references omitted integration-tagged callers; these explicit test callers are required.

Fixtures migrating Link directly: links `testStore`; customer tests at lines 28/97/180/210/282; management newApp/failure fixture; auth newAuthDatabase. Ensure both Link and Student/FK are migrated; explicitly list both in model fixture lists. Migration contract gets its own test starting with old schema, then invoking production links.Migrate.

Update link progression/name-isolation/cancel-all tests to protect finished links and students while retaining independent draft cancellation. Customer final-POST replay becomes 200; late draft stays 409. Replace completed-link cancellation/404 expectation with separately canceled unfinished link. Remove impossible canceled-completion fixture from TestRegistrySeparatesTrash; deliberate completed fixtures must also create student or use Submit before moving timestamps.

Extend management writePaths with `/management/students/1/fields/name`; cover new GET routes for signed-out/expired sessions and writes for both cross-origin header paths. Retain token secrecy, HEAD immutability, clipboard sequencing, row-local cancel and retryable failure contracts. Replace incidental wording/source-text assertions with persisted state, status and user-visible DOM behavior, not re-pinned copy.

## Critical files & anchors

- `internal/testdb/testdb.go:19-78`, `Open`: isolated real PostgreSQL schemas; do not replace with mocks or mutate shared dev data for automated tests.
- `internal/auth/http.go:81-102`, `Require`: HTMX session loss is 401 with HX-Redirect, not a fragment.
- `static/management.js:27-68`: clipboard request sequencing must remain link-local when extracting only time formatting.
- `internal/customer/customer_test.go:209-412`: real-wire HEAD contract must survive customer-content changes.

## Verification

Planning ran no migrations, tests or writes to application data. All checks below are execution-phase requirements.

### Prerequisites and commands

Run from `/home/gabrielvelez/projects/PaymentWebsite`, using existing Go toolchain and Fish. Reuse listener on 127.0.0.1:3000; only if absent start the single Air server after database readiness. Existing `.env` must contain DATABASE_URL and SESSION_COOKIE_SECURE=false; preserve unrelated settings.

```fish
docker compose up --detach --wait --wait-timeout 90 db
```

Confirm dedicated test DB exists with `docker compose exec -T db psql -U paymentwebsite -d postgres -Atc "SELECT 1 FROM pg_database WHERE datname = 'paymentwebsite_test'"`; only if absent run `docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test`.

```fish
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
go test -race -tags=integration ./internal/links ./internal/customer
go test -race -tags=integration ./internal/management ./internal/auth ./internal/students
go test -race -tags=integration ./...
```

Run first command for Phase 1, second for management phases, full suite after integration; do not repeatedly run unchanged checks. Plain go test without integration tag does not exercise existing database contracts.

### Permanent critical regressions

Use current real-PostgreSQL helpers/build tags and existing test files; no framework or broad mock suite.

1. **Phase 1:** fresh and old-schema cutover, then repeated migration preserves new rows and never resets auth/settings. Draft creates no student. Two simultaneous final submissions produce exactly one student and one immutable final answer; student timestamp equals SubmittedAt and ExpiresAt minus SubmittedAt is exactly 168 hours. Force student INSERT failure using a test-schema CHECK constraint rejecting one chosen name; assert whole submission rollback, original draft/expiry unchanged, HTTP 500 retryable form, and successful retry after removing that test constraint.
2. **Phase 1:** single/bulk cancel cannot change a finished entry or student. Concurrent submit/cancel has only two valid outcomes: canceled with no student, or Finished with exactly one student and no cancellation. Replay final POST is 200 with no writes/deadline extension; late draft is 409; valid finished GET/HEAD is 200, expired finished GET/HEAD is 404 and student remains.
3. **Phase 2:** fixed-time Archived table cases at 24h minus 1ns, exactly 24h, and over 24h, plus nil/unfinished. Authenticated HTTP pages omit archived finished records only from main, retain them in Students, not Trash; stale single-cancel fragment cannot reinsert them. Creation time is irrelevant. Empty Students page differs from database failure.
4. **Phase 3:** authenticated read → edit invalid Unicode/NUL/blank → save `María O'Connor 李` → reload; validation changes no row, valid save changes only student's Name, not original link snapshot/source/timestamps. Unknown fields (`id`, `source_link_hash`, `created_at`, SQL-like names), unknown IDs, malformed and >4KiB bodies cannot mutate data. Markup input stays escaped. Preserve unrelated student/field values.
5. **Phase 3:** new read/write paths leak no data to signed-out/expired sessions; unsafe cross-origin requests receive 403. 422 returns field editor with attempted value; failed DB read/write has no saved field/OOB success. Confirm ordinary fallback redirect versus HTMX fragment contracts.

### Actual browser smoke

Use browser tool against existing Air app, not only httptest. Reuse a known test session; if unavailable, create a uniquely named temporary staff account via `go run . user add <unique-smoke-name>` with DATABASE_URL exported and hidden interactive password prompts. Use a generated local password; never change existing staff credentials. Login at `/management/`.

1. Create two links through management, retain their one-time URLs. Autosave `Student smoke` on one; Students still empty for that link. Submit it; observe HTTP 200, Thank you heading, completion title, no input. Refresh URL: success remains. Completed management row has no pencil/cancel confusion; cancel the other draft and confirm only it enters Trash.
2. Students shows submitted Name and Submitted time immediately. Click a non-link cell and keyboard-activate Name; both open centered native dialog. Pencil Name, enter whitespace and Save: observe server 422 with inline error and retained dialog. Save `Student smoke edited`: field and row change without reload. Cancel a later unsaved edit; persisted name stays. Reload and verify edit persists.
3. Close with Close and Escape; focus returns to correct row after its OOB replacement. Repeat at 390px and desktop width; capture screenshots proving centered bounded dialog, visible controls and no page overflow. With JavaScript disabled, verify ordinary detail/edit forms remain usable.
4. Age only this smoke link with local psql (`docker compose exec -T db psql -U paymentwebsite -d paymentwebsite -v ON_ERROR_STOP=1`), using its actual 64-hex digest from the freshly generated row's cancel action. Execute the SQL below via stdin, replacing `<digest>`; no other rows may change. Reload: absent main, present Students, token success still 200. Repeat SQL with `169 hours` instead of `25 hours`: customer GET/HEAD 404, student still readable/editable, no Trash row.
5. Inspect console/network for JS/HTMX errors. Exercise a failed field request (browser network interception) and expired management session: unsaved inputs survive network error; session expiry navigates to login, not into modal. Recheck creating/copying/canceling links preserves unrelated one-time URLs. Close owned browser tabs after proof.

```sql
BEGIN;
UPDATE links
SET submitted_at = now() - interval '25 hours',
    expires_at = now() - interval '25 hours' + interval '168 hours'
WHERE token_hash = decode('<digest>', 'hex');
UPDATE students AS s SET created_at = l.submitted_at
FROM links AS l
WHERE s.source_link_hash = l.token_hash
  AND l.token_hash = decode('<digest>', 'hex');
COMMIT;
```

## Sources for framework-specific seams

- [HTMX response handling / beforeSwap](https://github.com/bigskysoftware/htmx/blob/master/www/content/docs.md): scope validation swaps instead of global error replacement.
- [HTMX table OOB swaps](https://github.com/bigskysoftware/htmx/blob/master/www/content/attributes/hx-swap-oob.md): wrap standalone tr in template.
- [HTMX request synchronization](https://github.com/bigskysoftware/htmx/blob/master/www/content/attributes/hx-sync.md): inherited shared-container queue all keeps field responses ordered.
- [GORM UPDATE RETURNING](https://gorm.io/docs/update.html), [migration](https://gorm.io/docs/migration.html), [FK constraints](https://gorm.io/docs/constraints.html): reuse installed GORM/PostgreSQL features.

## Assumptions & contingencies

- User selected next-page-load archival and seven days from completion; Students represents completed submissions, not merged people.
- Original link data remains a submission snapshot; staff edits canonical student data only. Future payment schema must not be implemented here.
- One-time loss of existing mock link data is accepted by repository policy; staff/session/settings data is not disposable for this cutover. If production-ready status changes before execution, stop destructive migration and obtain a preservation decision.
- No dependency, Go toolchain, build/deploy config change is needed. Do not push or deploy without separate approval. Reuse existing Air server; if no listener remains, start exactly one after Compose DB and .env readiness.
- Backend error paths use real PostgreSQL tests; browser network interception proves UI error retention only, not successful persistence. If CDN/browser access is unavailable, finish reachable HTTP/database proof and explicitly report missing visual verification rather than claiming it.

