# Synchronous DB request ownership cleanup

## Context

Aggressively decomplect the entire PaymentWebsite repository, with customer-form DB latency as the priority: reduce code, untangle ownership, merge inseparable machinery, and delete obsolete code without weakening security or data integrity. The user chose **simplify synchronous SQL first**, not goroutine batching or write-behind. The user has already moved PostgreSQL East alongside the app; no region move belongs in this implementation. End state: fewer sequential DB commands, committed acknowledgements, one owner for each lifecycle, and no application cache or write queue.

Repository: `/home/gabrielvelez/projects/PaymentWebsite`. Reports: `/tmp/paymentwebsite-capacity/report.md`, `report.json`, `protocol.json`, `workload.js`, `run-stage.js`. Historical cross-region report establishes no passing capacity: 15 active users had p95 739.5 ms/p99 1146.4 ms; larger runs hit five-second DB deadlines. Pool contention was inferred, not measured. Those timings are not a valid pre-code baseline after co-location.

## Approach

### 1. Establish the comparable baseline before editing

Integration owner owns `main.go`, `internal/testdb/testdb.go`, shared callsite integration, and final verification. Reuse the existing Air server at `http://127.0.0.1:3000`; it was reachable during planning. Never start a second Air/dev server. If nothing listens during execution, start the Compose DB and one `go tool air` from repository root; `.env` supplies `DATABASE_URL` and `SESSION_COOKIE_SECURE=false` without opening its contents.

Run the local before-change workload described under Verification before any application edits; retain aggregate measurements for identical after-change runs. Do not rerun the old cross-region failures to confirm the supplied report. Existing Railway topology now differs by user report; any later Railway comparison must measure both revisions with app and DB co-located and unchanged resources.

Use existing Go/GORM/PostgreSQL/SCS/HTMX dependencies; no new dependencies, queue framework, Redis, metrics endpoint, pool-size expansion, pagination, or field collection feature. PostgreSQL remains authoritative for link usability, saved drafts, finalization, settings and staff account revocation. Customer responses retain `Cache-Control: no-store` and `Referrer-Policy: no-referrer`; caching bearer-token state would introduce invalidation and restart semantics this change does not need.

### 2. Make database operations and their outcomes belong to the link store

This package and its customer caller migrate together. Targets: `internal/links/links.go`, `internal/customer/customer.go`, their tests, and direct test consumers listed below.

**Transaction policy.** In `main.go`'s existing `gorm.Config`, set `SkipDefaultTransaction: true`. Do the same in the single-pool `testdb.Open` change below. Current runtime writes are single SQL statements except final enrollment; PostgreSQL statement atomicity remains intact. Preserve explicit transactions in failure-injection tests and wherever a genuine multi-statement unit remains. Do not set the flag again per store. Keep `SetMaxOpenConns(10)`; change `SetMaxIdleConns(5)` to `SetMaxIdleConns(10)`, without adding an unevidenced connection-lifetime policy.

**Authoritative save result.** Add in `internal/links/links.go`:

```go
type SaveResult uint8

const (
    Unavailable SaveResult = iota
    Saved
    AlreadySubmitted
)
```

Change both signatures to:

```go
func (s *Store) SaveDraft(ctx context.Context, token, name string) (SaveResult, error)
func (s *Store) Submit(ctx context.Context, token, name string) (SaveResult, error)
```

`Unavailable` means malformed/unknown/canceled/expired; `Saved` means this operation committed; `AlreadySubmitted` means the still-usable link was previously finalized. Database errors return `Unavailable, err`, never a successful result. Keep token length checking and SHA-256 digest storage unchanged; raw tokens never enter persistence or logs.

`SaveDraft` keeps its guarded single `UPDATE name` using `active(ctx, digest).Where("submitted_at IS NULL")`; empty drafts remain valid and never enroll students. Introduce one unexported `saveResult(ctx context.Context, digest []byte, result *gorm.DB) (SaveResult, error)` shared by the two writers: return error first; one affected row means `Saved`; on zero rows only, query the active digest with `Select("submitted_at").Limit(1).Find(&link)`, returning `AlreadySubmitted` only for a usable submitted row, otherwise `Unavailable`. Pass the already-computed digest, not the raw token: no repeated hashing and no new cache. The fallback is deliberately an authoritative second statement after a no-op; do not add locking/UNION CTE machinery merely to force every replay into one command.

**Atomic completion in one command.** Replace `Submit`'s interactive `Transaction` callback with `s.db.WithContext(ctx).Exec` using bound `name, digest` and this exact PostgreSQL operation:

```sql
WITH completed AS (
    UPDATE links
       SET name = ?,
           submitted_at = now(),
           expires_at = now() + interval '168 hours'
     WHERE token_hash = ?
       AND canceled_at IS NULL
       AND expires_at > now()
       AND submitted_at IS NULL
    RETURNING token_hash, name, submitted_at
)
INSERT INTO students (source_link_hash, name, created_at)
SELECT token_hash, name, submitted_at FROM completed
```

Pass its GORM result to `saveResult`. The final command tag counts inserted students. A uniqueness, FK, CHECK or deferred commit constraint error must fail the entire command: no completion without enrollment, no changed draft/expiry on failure, and no acknowledgement before the driver reports success. Keep `Link.Student`'s GORM FK declaration, the unique `Student.SourceLinkHash`, and the independently editable student table. No equivalent single-command enrollment currently exists; this SQL replaces the multi-command implementation, not a second path.

**First visits and cancellation.** Keep `Lookup` read-only and `Open` separate for HEAD semantics. `Open` returns the initial lookup immediately when already opened; otherwise use its returned digest with `Model(link)`, `opened_at IS NULL`, and `Clauses(clause.Returning{})` to conditionally stamp first visit and return the populated row. On update error return error; on zero affected rows call `Lookup` again to observe the concurrent open/cancel/expiry instead of returning stale access. Repeat GETs must remain SELECT-only, not unconditional updates.

In `Cancel`, preserve staff validation and management hex-digest parsing. Use guarded `Updates(canceledBy(staff))` with `Model(&link)` and `clause.Returning{}`; successful cancellation uses that returned row without another query. On zero affected rows reset `link = Link{}` before the existing unfiltered lookup: GORM can assign map values to the model before SQL, so stale `CanceledBy` must not leak. Finished/expired/already-canceled rows retain their existing audit and return their current entry; unknown/malformed IDs remain nil. `CancelAll` retains its single guarded database update.

**HTTP ownership.** Keep `RegisterRoutes`' public signature and existing templates. For POST, cap body at 4 KiB and parse/validate in memory first with `students.ParseName(data.Name, !draft)`. Valid input calls the selected writer immediately: no initial `Lookup` and no HTTP-layer post-save lookup. Map `Saved` to draft status-only 200 or final confirmation 200; map `AlreadySubmitted` to final 200 or draft 409; map `Unavailable` to 404; database errors to existing retryable 500 with attempted name retained. Preserve the same HTMX full/fragment selection, refresh on 404/409, completion-name privacy, and `Vary: HX-Request`.

For invalid POST input, perform `Lookup` before rendering the parse/validation error: unusable links still give 404; usable completed links still give 200 final replay/409 stale draft even for malformed or oversized bodies. Only active unfinished links receive 400/413/422. On 400/413 restore saved draft name from the row; on 422 retain attempted name. GET calls `Open`; HEAD calls `Lookup`, writes headers/status only, and never marks a visit. No POST marks a first visit. Correct the existing README claim that POST sets `opened_at` when documenting this changed flow.

**Caller cutover.** Production method-value callers are `customer.go`'s `save := store.Submit`/`store.SaveDraft`. Migrate all direct and method-value test consumers in `internal/links/links_test.go`, `internal/customer/customer_test.go`, `internal/management/management_test.go`, `internal/management/students_test.go`, and `internal/students/students_test.go`: success is `== Saved`; completed replay is `== AlreadySubmitted`; unusable is `== Unavailable`; errors never accompany `Saved`. Update callback/result structs currently typed `(bool, error)`, especially `refused`, concurrency result channels, and method tables. Exact exhaustive literal search: `\b(SaveDraft|Submit)\b` across `internal/**/*.go`; production LSP references and integration-tagged textual references were both checked during planning. No bool shim/alias remains.

Acceptance: successful draft POST is one guarded UPDATE; successful final POST is one atomic CTE command; a repeat GET is one SELECT; first open is SELECT + conditional UPDATE; successful cancel is one UPDATE RETURNING. These are intended command counts, not measured speedup claims. Existing rollback/race and HTTP precedence tests must still pass with explicit new outcomes.

### 3. Give auth sole session ownership and make process teardown real

Can run independently of student presentation after step 1; integration owner merges `main.go` and shared fixtures. Targets: `internal/auth/http.go`, `main.go`, `internal/auth/http_test.go`, `internal/management/management_test.go`.

Change constructor to `func New(db *gorm.DB, secureCookies bool) (*Manager, error)` and add `func (m *Manager) Close()`. Move SCS construction and cookie configuration from `main.go` into `auth.New`; add `store *postgresstore.PostgresStore` to `Manager`. Parse the template, build the dummy hash, and obtain `db.DB()` before creating the store, so constructor failures cannot leak its cleanup goroutine. Retain `postgresstore.New(sqlDB)` and its periodic expired-session deletion; `Close` calls `StopCleanup` exactly once per successfully constructed manager. Auth does not own/close the SQL pool.

Move `cookieName = "management_session"` from `http_test.go` into `http.go`. Preserve lifetime 14 days and cookie path `/management/`, Secure from argument, HttpOnly true, SameSite Lax, Persist true, Domain empty, IdleTimeout zero. Retain explicit security-relevant cookie assignments rather than depending on changed defaults.

`RegisterRoutes` wraps each login/logout handler in `m.sessions.LoadAndSave`; `Require` wraps its returned protected handler once. Root mux, customer routes, static assets and favicon bypass SCS. `main.go` retains outer `http.NewCrossOriginProtection().Handler(mux)`. Keep fresh token rotation, dummy-password work, login limiter, per-request `AuthVersion` lookup, CLI revocation, health-check sign-in response, and HTMX unauthorized redirects. Keep `revokeSessions` as a domain function: inlining it would leak column names and crypto mechanics into CLI dispatch.

Migrate the only constructor consumers: `main.go`, `internal/auth/http_test.go:newApp`, `internal/management/management_test.go:newApp`. Delete their duplicated SCS/postgresstore wiring and outer `LoadAndSave` wrappers. Main defers `authentication.Close()` after successful construction. Tests register `t.Cleanup(manager.Close)` before server-close cleanup, so LIFO order is HTTP server → auth cleanup → schema drop → DB close.

In `main.go:run`, retain argument parsing and introduce `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`. Derive the ten-second startup context from it; pass signal context to `auth.RunCLI`. Add unexported `serve(ctx context.Context, server *http.Server) error` to own HTTP lifetime: buffered one-result channel for `ListenAndServe`, select server error versus signal cancellation; on signal use a fresh background ten-second shutdown context, call `Shutdown`, fall back to `Close` on shutdown failure, and consume the serving result. Treat `http.ErrServerClosed` as expected, preserve other serving/shutdown errors, and return so auth cleanup precedes SQL close. Do not use the signal context as HTTP request BaseContext: graceful drain must not immediately cancel active requests. No store Close/flush API exists because there is no write worker. Keep `.air.toml` unchanged; Air's forceful rebuild behavior is not a production shutdown guarantee.

Acceptance: ordinary public requests do not allocate/load staff sessions; a forged staff cookie on a public request cannot trigger session DB access. Staff sessions survive manager recreation and expire/revoke as before; expired session rows still get cleaned. SIGTERM permits already-running handlers to finish before pool closure, within the existing shutdown budget.

### 4. Remove obsolete schema machinery and duplicated test resources

Integration owner can combine this with step 2's `main.go`/testdb edits. `internal/testdb/testdb.go:Open` retains its signature and dedicated `TEST_DATABASE_URL` validation, but opens one schema-scoped GORM/SQL pool instead of `base/admin` plus `db/pool`.

Generate `pwtest_` + 8 random bytes encoded as hex first; place that generated identifier in DSN `search_path` before opening the pool. Set DisableAutomaticPing and SkipDefaultTransaction true; max open/idle both 10. Immediately register pool-close cleanup. Under existing ten-second setup context, run `CREATE SCHEMA` with the internally generated identifier; this also proves connectivity. Only after success register `DROP SCHEMA ... CASCADE` cleanup using a fresh background five-second context, then run requested AutoMigrate. Drop errors remain test failures; migration failure still drops the created schema; schema-create failure closes the pool without attempting a drop. Delete both redundant pings and the admin pool. Caller cleanup registration remains unchanged except step 3's manager cleanup.

Delete `links.Migrate` and replace the split startup migrations with one `AutoMigrate(&auth.User{}, &auth.Session{}, &settings.Settings{}, &links.Link{}, &students.Student{})`. Delete `TestMigrateCutsOverOnce`, but first move its FK delete/update rejection assertions into `TestStatusFollowsCustomerProgress` after completion, using a fresh nonconflicting replacement digest. Preserve the repeat-AutoMigrate retention check there: take completed link/student snapshots, AutoMigrate current models twice, assert rows unchanged. Delete only legacy ciphertext/cutover/sentinel scaffolding and now-unused auth/settings imports from `links_test.go`.

Delete completed `STUDENTS_ARCHIVE_PLAN.md` and unused `migrations/.gitkeep`; startup uses AutoMigrate, not that empty directory. No versioned migration framework replaces them. Keep generated Air artifacts and module checksum files under their existing owners; do not hand-edit them. References to the retired files/cutover in README follow the implementation cutover, not a second source of architecture.

Acceptance: fresh isolated schemas work; every existing test still isolates itself; teardown releases its sole pool and drops only its generated schema; retained source-link FKs reject deletion/hash changes; current-model remigration preserves rows. If an obsolete local mock volume cannot migrate, reset only its `students`/`links` tables explicitly as a one-off development operation rather than restoring compatibility code. Do not reset live/shared data without separate authorization.

### 5. Consolidate presentation data without erasing real boundaries

Can run in parallel with step 3 after step 1. One presentation owner owns `internal/students/fields.go`, `internal/students/students.go`, `internal/management/students.go`, `internal/management/management.go`, management templates and touched static files. Integration owner handles their overlap with shared test fixtures and links status changes.

**Field metadata.** Keep pure `fields.go` and non-integration `fields_test.go` separate from SQL persistence. Replace six-method `Field` interface and sole `nameField` implementation with:

```go
type Field struct {
    Key, Label, InputType string
    Required bool
    Value func(Student) string
    Parse func(string) (any, error)
}
```

Registry's sole entry remains Key `"name"`, Label `"Name"`, InputType `"text"`, Required true, Value returning `Student.Name`, Parse calling `ParseName(raw, true)`. Retain `Fields() []Field` and `FindField(key string) (Field, bool)`; missing field returns `Field{}, false`. Registry stays package-owned/read-only by existing caller convention, not externally extensible; keep allowlist lookup in `Store.UpdateField` before SQL. Replace getter calls with fields in `students.go:UpdateField` and `management/students.go:studentDisplay/fieldHandler`; preserve ParseName/ValidationError and SQL-error PII suppression. Keep external `package students_test`: importing links from in-package students tests creates a cycle.

**Views and templates.** Embed `students.Field` in `studentFieldView`, retaining StudentID, rendered Value string, Modal, Editing, Error, Saved. Embed `students.Student` in `studentView`, retaining Fields and OOB. `studentDisplay` copies the record once, builds its Fields once, and calculates each rendered Value once. Merge `studentListData` into `studentPageData` with Staff, Fields, Students, Student, Field, Modal. Keep Modal (dialog context) and OOB (row replacement) distinct.

Merge `templates/student.html` into `templates/students.html`: one existing head/header/navigation/main shell; select detail vs list via `.Student.ID`; detail branch has its back link and detail panel, list branch retains table/empty state and native dialog. Titles remain Student details/Students. Update the two Go `"student.html"` executions to `"students.html"`; preserve `student-detail`, `student-field`, `student-row`, and `student-save` fragments. Inline only `student-field-read` and `student-field-editor` within `student-field`'s existing Editing branch; keep `student-save` as owner of saved field plus optional OOB row. Delete the old template file. Remove unreachable ErrUnknownField handler branch after successful FindField; retain explicit ParseUint error handling and record-before-body precedence.

**Request state and dead hooks.** Keep `static/students.js`'s WeakMap: it snapshots field/row identifiers before outerHTML/OOB swaps detach nodes. Delete only the never-used error paragraph in `student-detail`, retaining list-level and field-level alerts. Replace `studentRequestError(request)` with direct field-ID lookup or `document.querySelector("main.management-students > .student-request-error")`. Remove unused `data-student-edit`, `student-field-error` class token (keep student-validation-error and error ID), and `student-link-...` anchor IDs (keep data-student-link and row IDs). Preserve all focus/error logic, native dialog, no-JS paths and data-student-id selectors.

Remove exact unused template class tokens management-registry, management-create, management-cancel-all and management-settings; do not remove settings-panel/form/footer classes. Delete unused CSS variables --brand-tint and --brand-border. Retain distinct student-empty CSS: unlike management-empty it has different layout and heading weight; switching classes would silently restyle the page for a small deletion. Keep management-time.js separate from link-specific management.js and keep clipboard request sequencing intact.

**Link rendering.** Remove pageData.Link and ShowLink. Change the local render closure to `(ctx context.Context, w http.ResponseWriter, r *http.Request, data pageData, single *links.Entry)`. Use one predicate `e.Trashed() == data.Trash && (data.Trash || !e.Archived(now))`. For an HTMX single-row operation, render zero or one entry in data.Links without loading/replacing unrelated rows. Otherwise load/filter the list once and overlay only the freshly created entry's one-time Token when supplied. Full-page data gets Staff; fragments do not need it. All create/cancel/list/Trash/cancel-all calls migrate in the same file; absent single is nil. Simplify `result` template to range over .Links then render existing OOB notice. Keep unknown HTMX cancel 204 and ordinary full-page fallback.

In links, change `entry` to `entry(now time.Time)`; `List` captures now once after reading rows and before projecting them, Create and Cancel pass one current instant after their successful DB operation. Make Cancelable explicitly true only for NotOpened, Opened, InProgress. Keep AccessExpired separate for completed success-page deadlines and Archived(now) for the 24-hour list policy. Backend cancellation still enforces database time, regardless of a previously rendered button.

In `showSettings`, accept the already-created request context rather than allocating another deadline on an invalid settings POST; GET creates its usual five-second context once. Keep database-backed settings, existing defaults/check constraints, and save/audit errors. Do not introduce a settings cache for this low-volume route.

Acceptance: list/detail/edit/modal/save responses retain their contracts and accessibility; unrelated rows keep one-time URLs after single cancel; staff filters agree with projected status; row OOB update and focus restoration still work. No UI redesign or broadened shared UI framework.

### 6. Keep regression tests focused while removing repeated machinery

Fold these edits into their owning packages, not a separate late test rewrite:

- `internal/customer/customer_test.go`: extract `newMux(t *testing.T, store *links.Store) *http.ServeMux` from the six repeated registration blocks and reuse from `serve`. Extract `requireFormError(t *testing.T, body, path string, fullPage bool)` for the two repeated retryable-error markup checks. The Existing name seed now earns its place: assert stored name on 400/413, attempted input on 422/save errors. Rename failedLookup fixture field/case to database failure, since valid POST no longer does a lookup.
- In `requireCompletion`, delete exact confirmation-copy/title wording assertions and the old question-string prohibition; retain one stable customer-content section, one title, full-page/fragment distinction, absence of editable form/input and absence of private submitted/canonical values. Existing rendered-markup checks that protect HTMX targets, accessible errors, escaping, auth boundaries or body limits remain.
- `management_test.go:TestUnauthorizedRequestsCannotUseManagement`: include `/management/trash` and `/management/settings` in its existing ordinary/HTMX protected-read table; delete duplicate Trash-only subtest.
- Consolidate duplicated multi-staff cancellation scenarios into `TestStaffShareLinkStatuses`: preserve existing creation/full-page/one-time-token checks, then carry over canceled-draft retention, repeat-canceler preservation, expired-row HTMX removal, unknown/malformed/raw-token ID behavior (HTMX 204 versus ordinary page), and HTMX cancel-all from `TestHTMXCancelMovesOnlyItsRowToTrash` before deleting that duplicate test. Keep named subtests for these distinct outcomes; don't mix archive editing or DB failure tests into this flow.
- `management_test.go:signIn` uses real login against a random-password hash generated with `&argon2id.Params{Memory: 16, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}` only in this test helper. Production and auth security tests keep current parameters. Do not cache shared mutable session/account fixtures.
- Keep distinct student successful-update, rejected-update, and database-failure tests separate: their independent fixtures make failures attributable. Do not compress invalid-ID/HTTP-method boundary coverage merely to lower query counts. Keep all enrollment rollback, submit/cancel race, after-race immutability, expiry and HEAD wire-body tests.

## Critical files and anchors

Line numbers are reread hints, not patch coordinates.

- `internal/links/links.go:20-39` — Link.Student association is migration metadata generating the retained source FK, not dead query state.
- `internal/links/links_test.go:263-278,366-424` — FK and current-model remigration checks must survive legacy-test deletion; deferred constraint test validates implicit CTE commit failure.
- `internal/customer/customer_test.go:303-400` — malformed/oversized completed replay and private-name suppression constrain parse-first request dispatch.
- `static/students.js:24-81` — request-origin snapshots survive detached nodes and distinguish normal/OOB swaps; retain this state.
- `/tmp/paymentwebsite-capacity/workload.js:27-48,117-133,171-267` — private fixture schema, workload timing, immediate reload and synchronized final submission define comparison workload.

## Verification

### Observed during planning

- `go vet -mod=readonly -tags=integration ./...` completed without diagnostics. Language-server workspace build diagnostics: no issues.
- Existing local Air surface: sign-in HTML fetched; HEAD `/static/styles.css` returned 200 with `Vary: Cookie`; HEAD of an unknown 43-character customer token returned 404, no-store, no-referrer and `Vary: HX-Request`.
- Six parallel decomplect audits covered all inventoried source/tests/config/docs; three independent rechecks rejected unsafe SQL and removals. Secret/env files remained unopened. Optional staticcheck/golangci-lint/deadcode/govulncheck/nix binaries are absent; none installed in read-only mode. Integration tests, proposed SQL and load checks have **not** been executed during planning.
- GORM behavior checked against installed v1.31.2 callbacks and official docs: https://gorm.io/docs/transactions.html and https://gorm.io/docs/sql_builder.html. No projected latency/capacity gain is an observed result.

### Executable Go checks

Working directory is repository root. Use the dedicated test DB, never production DB, for integration tests. If absent, create it after Compose is running with `docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test`; an existing DB is reused, not recreated. The documented local test URL is nonsecret development configuration:

```fish
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
go test -race -tags=integration ./...
```

Run once after integrated edits, not separately in concurrent packages. Focus failures without repinning incidental expectations. In particular verify:

1. `TestConcurrentSubmissionEnrollsOnce`: exactly one Saved and one AlreadySubmitted; exactly one student; equal completion/enrollment times; expiry exactly 168 hours later.
2. `TestSubmissionFailureRollsBack`: both immediate CHECK and deferred UNIQUE failure return error/non-Saved, preserve draft/expiry, and allow retry after constraint removal. Do not weaken this test if raw Exec reports errors differently.
3. `TestSubmissionCancellationRace`: either saved completion + untouched cancellation + one student, or canceled link + Unavailable submit + no student.
4. Extend `TestStatusFollowsCustomerProgress` to assert Open returns populated first-visit timestamp and repeat Open preserves it. Add `TestOpenObservesConcurrentCancellation` in `internal/links/links_test.go`: an explicit transaction updates the fresh row to canceled but does not commit; obtain its `pg_backend_pid()`. Start Open in another goroutine so initial SELECT sees the old usable row and conditional UPDATE waits. Under a bounded context, poll `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE ? = ANY(pg_blocking_pids(pid)))` with the blocker PID until waiting is observed; commit cancellation, then require Open returns nil and the row stays canceled. Always roll back/release blocker on test failure. No timing-only sleep and no mocks. Integration owner runs this targeted regression once against old Open before fixing it, then final suite proves it passes.
5. Existing customer tests: stored name retained for malformed/oversized active input; invalid UTF-8/NUL rejected; completed malformed/oversized replay wins over parsing errors; stale draft 409/HX-Refresh; canceled/expired 404; HEAD has zero body and changes no data.
6. Auth tests plus `TestPublicCustomerIgnoresStaffSessions` in `internal/auth/http_test.go`: register actual `customer.RegisterRoutes(mux, links.New(db))` in its existing `newApp` root mux, matching production separation. In the test's isolated schema rename `sessions` to `unavailable_sessions`; send a fabricated nonempty `management_session` cookie to a well-formed unknown-token customer HEAD and require normal 404, while protected `/management/` reports session-store failure. This protects public token access from unrelated staff-session failures; no fake endpoint or cookie secret is required. Keep origin-protection checks and verify actual static resources in live browser smoke rather than adding a synthetic static test handler.
7. Throwaway `TestServeLifecycleSmoke` in repository-root `main_test.go` exercises `serve` under `go test -run '^TestServeLifecycleSmoke$' .`: use an isolated loopback test listener/port, hold an in-flight handler on a channel, cancel lifecycle context, release handler, require successful complete HTTP response and nil serve result; also occupy its port to exercise immediate listen error. Keep bounded test deadlines and remove harness after proof. This is an isolated test server, not a second dev/Air instance; production route smoke below still uses Air.

### Browser and actual route smoke

Reuse `http://127.0.0.1:3000`; browser tool opens and closes its verification tabs. Use a disposable local staff identity created through existing auth functionality; supply credentials through browser privately, never print/read secret files. If no browser login exists, run `env DATABASE_URL='postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite?sslmode=disable' go run . user add decomplect-smoke-<unique-suffix>` through PTY and its existing hidden password prompt; remove only that temporary account afterward through `user remove`. Do not add a test-login endpoint or bypass auth. The URL shown here is the inspected Compose development configuration, not a live secret.

- Create two links; autosave `Ana Pérez 李`, reload and observe exact canonical value without student enrollment. Clear draft, reload empty. Submit `Ana Pérez 李`: success replaces form and Students gains exactly one row. Repeat final POST with malformed body: confirmation and no changed record; stale draft: 409 plus refresh; cancel other link: subsequent customer request 404.
- In Students, test row pointer and keyboard opening, Escape/Close and focus return after saved OOB row replacement, Unicode edit, whitespace-only name 422 retaining input, page-level failed-load and field-level network/500 alerts, and standalone no-JS edit/save/cancel. Verify existing labels, validation associations and focus indicators remain; capture visual evidence. Check empty-state surface if already empty; otherwise preserve unrelated rows and rely on existing `TestStudentEmptyAndDatabaseFailures` for its rendered empty-state contract (empty styling is intentionally unchanged).
- Single-link cancellation must not erase another row's still-visible one-time URL; delayed clipboard completion must not replace newer cancellation feedback. Settings range and creating/canceling staff attribution remain observable.
- Exercise invalid/canceled HEAD and verify no body; static GET with forged staff cookie must work without a session DB dependency. No form/private-data browser caching introduced.

### Before/after performance proof

Required local pair; deployment is not implied by plan approval. Preserve historical reports untouched. Use new temporary directory `/tmp/paymentwebsite-decomplect` and existing unmodified `/tmp/paymentwebsite-capacity/workload.js`. Do not use `run-stage.js` for local runs: it hardcodes Railway telemetry IDs.

Before application edits, run 15 and 62 VUs, twice each; after edits repeat exact stages against same Air server/DB/topology/resources. Use identical `RUN_ID=DBDECOMP` and stage IDs `15-a`, `15-b`, `62-a`, `62-b` in both phases: workload seeds think times from RUN_ID plus STAGE_ID, so adding before/after to STAGE_ID would change the workload. Store results separately in `/tmp/paymentwebsite-decomplect/before` and `/tmp/paymentwebsite-decomplect/after`. DURATION_SECONDS=120, SUBMIT_AT_SECONDS=105, existing 1–2-second seeded think time, 15-second graceful completion and unchanged semantic checks.

A throwaway Bun fixture/runner script must use `node:crypto` randomBytes(32), base64url tokens and SHA-256 IDs; write mode-0600 JSON array of `{path:"/customer/<token>",id:"<64 lowercase hex>",name:"Capacity Student <index>"}` with exactly VUS entries and the same names/index ordering across phases. Seed matching digest rows through `docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U paymentwebsite -d paymentwebsite`, using a unique phase/stage/run nonce in created_by and expiry now()+24 hours. Names/markers are generated safe ASCII, not user input. Never emit paths/tokens/credentials to reports or tool output; raw fixture file is runtime input, not something to open in inspection tools.

Create the phase results directory before the stage. Run each stage with the pinned image and parameters from supplied runner (substitute stage/VUS and only the results directory for phase); matching host UID/GID lets k6 read the private fixture and write results:

```fish
docker run --rm --network host --user (id -u):(id -g) --cpus 16 --memory 12g --memory-swap 12g --security-opt label=disable --ulimit nofile=1048576:1048576 \
  --env BASE_URL=http://127.0.0.1:3000 --env VUS=15 \
  --env RUN_ID=DBDECOMP --env STAGE_ID=15-a \
  --env DURATION_SECONDS=120 --env SUBMIT_AT_SECONDS=105 \
  --volume /tmp/paymentwebsite-capacity/workload.js:/scripts/workload.js:ro \
  --volume /tmp/paymentwebsite-decomplect/fixtures.json:/fixtures/fixtures.json:ro \
  --volume /tmp/paymentwebsite-decomplect/before:/results \
  grafana/k6@sha256:e66db15b860113878fa74670e31f5e274830b7b6e42c8bff28b2f2d86a257603 \
  run --quiet --log-output=none --no-usage-report /scripts/workload.js
```

After each stage, reconcile only exact seeded digests: all persisted completions correspond one-to-one to students; both stored names equal that fixture's `Capacity Student <index>` final name; enrollment created_at equals submitted_at; expiry minus completion is 168 hours; unfinished rows have no student. A failed final HTTP can have an unknown committed outcome, so reconcile persisted rows without falsely requiring HTTP-success count to equal DB-completion count. Report aggregate counts and errors only. Remove stage-owned students before stage-owned links by exact digest list plus marker, then prove zero fixture rows remain; this isolation/cleanup is part of measurement, not permission to delete unrelated data. Treat k6 exit 99 as a measured threshold failure, other nonzero exits as runner failures; neither bypasses reconciliation.

Compare aggregate and per-route p50/p95/p99, request count, HTTP error rate, completion count and integrity results. Required correctness: semantic/integrity errors zero, >=99% completed, HTTP failures <=1%; retain original latency budgets p95<=500ms/p99<=1000ms. Report observed before/after and any misses honestly; do not infer a maximum user capacity or assume local timings establish Railway capacity. If unchanged-region runs still regress/miss budgets, inspect route breakdown and errors before any new cache/queue/pool design; those are not automatic fallbacks.

For live Railway proof after separate deployment authorization, repeat the same workload before/after code on the already-co-located environment, keeping 1 replica/1 vCPU/4GB and DB config fixed. Known scope: project `a0207263-4a1b-4174-b693-5830fd46e486`, environment `fec56217-60f4-4bd6-85c6-84237b096cde`, app `fcee069c-48a8-43eb-ad79-91fe5341d4c1`, DB `4032819a-442c-4caa-affc-b2ee2f796bc2`. Reuse supplied run-stage/workload; regenerate removed private fixtures via Railway SSH/psql without displaying secrets, record exact revisions/regions/deployment IDs, verify deployment SUCCESS/logs/live route smoke, and reconcile exact test rows. No production push, infrastructure mutation or remote fixture write occurs without that authorization. Without it, deliver local evidence and explicitly state Railway post-change performance remains unmeasured.

## Assumptions and contingencies

- User decision: synchronous committed writes; no volatile drafts, batching goroutine, application cache, or topology change. Co-location East is user-reported completed work, not a finding to re-confirm by rerunning old failures.
- Existing mock-data policy permits simple local resets rather than compatibility/backfill code. Do not reset live/shared data or delete unrelated rows as part of benchmarking.
- All application callers are repository-owned. Migrate signatures atomically with their tests; no compatibility shims. If intervening user edits add callers/hooks/associations, reread them and include them in the same ownership migration; preserve explicit transactions for newly discovered multi-statement units rather than blindly applying SkipDefaultTransaction to them.
- Keep PostgreSQL authoritative across replicas, CLI processes, and restarts. No single-replica cache assumption is embedded in the design.
- Live deployment remains separately approval-gated. No go.mod/go.sum/toolchain/build/deploy dependency change is planned; if a genuinely required server dependency change appears, stop that portion for the repository's required non-production Railway preview before merge/push rather than silently expanding scope.
