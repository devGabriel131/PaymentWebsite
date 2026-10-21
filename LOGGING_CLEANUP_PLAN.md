## Context

Clean up logging throughout `/home/gabrielvelez/projects/PaymentWebsite`, not merely its diff: compact structured events, bounded slow-query summaries, request correlation, safe failure detail, and clear lifecycle outcomes. Railway's 500-lines/second limit was exceeded and 21,167 messages were lost, per the user's observation; do not reproduce that flood against Railway. Preserve HTTP behavior, authentication, privacy, synchronous persistence, and SQL atomicity while deleting duplicated logging policy and endpoint-specific workarounds.

The user chose **keep SCS PostgreSQL persistence; own only expiry cleanup**, and **logging slice only**. Reuse Railway HTTP logs for traffic/latency/error-rate investigation; pool-wait metric export and alert provisioning are explicitly deferred. No logging service, background log queue, extra access-log stream, schema/dependency/toolchain change, or deployment is part of this implementation.

## Established model and coverage

- `main.go` owns one SQL pool, GORM configuration, startup migration, route assembly, and the 10-second graceful HTTP drain. Keep those owners and their existing pool/timeouts/transaction choices.
- GORM's current Warn logger emits multiline SQL and raw database errors. Seventeen HTTP `log.Printf` sites then apply inconsistent failure/privacy policy: customer 3, management 10, auth 4. SCS adds its default request `ErrorFunc` and postgresstore's independent raw-logging expiry worker.
- Stores already accept and propagate request contexts and return errors. Keep `links`, `settings`, and student domain behavior; only remove the student-update-specific logging override once the central logger is safe.
- SCS copies requests and may write 500 during session commit before the handler's intended status. Inner management mux patterns therefore cannot be recovered by reading only the outer request after dispatch; actual committed status cannot be inferred from handler intent.
- Four parallel subsystem audits covered runtime/configuration, auth/CLI, HTTP/UI consumers, and persistence/tests. Safe path inventory and exclusions are recorded in `local://logging-cleanup-inventory.md`. Templates/static assets have no logging implementation to change. `go.sum` belongs to Go tooling; `tmp/` belongs to Air; `migrations/` has no files.
- Never open `.env*`, keys, credentials, secret-named files, runtime logs containing private data, or git-crypt-protected files. No `.gitattributes` was found. Credential-bearing `compose.yaml` content was excluded; its runtime role is documented in README.

## Approach

### 1. Establish one small logging boundary

**Integration owner; prerequisite for all cutovers.** Add `internal/logging/logging.go`, `http.go`, and `database.go`. No equivalent exists in this repository. Keep three responsibilities in three files within one package; do not introduce a generic observability framework, options/factory hierarchy, SQL parser, exporter, or pluggable logging interfaces.

Public API for the cutover:

```go
func New(out io.Writer) *slog.Logger
func ErrorAttrs(err error) []slog.Attr
func ServerErrorLog(logger *slog.Logger) *log.Logger
func Requests(log *slog.Logger, next http.Handler) http.Handler
func Operation(name string, next http.Handler) http.Handler
func RecordError(r *http.Request, stage string, err error)
func WithOperation(ctx context.Context, name string) context.Context
func NewDBLogger(log *slog.Logger) *DBLogger
func (l *DBLogger) Flush()
```

`DBLogger` implements `gorm.io/gorm/logger.Interface`. Use the standard `*slog.Logger` directly at lifecycle/background boundaries, not another application logger wrapper. `Requests` receives an explicit logger; tests inject their own sink instead of changing process-global loggers. No `slog.SetDefault` or global logging output redirection is needed.

Implement the shared files completely using the HTTP/database contracts in steps 2–3 before consumer cutovers; the API list is not permission to add stubs. The integration owner owns these files and `main.go`. Parallel consumer owners edit only their assigned auth or customer/management files; their main/constructor wiring lands atomically with the integration owner.

Net effect is fewer logging owners and deletion of repeated formatting/privacy policy, not a promised repository-wide line reduction. New shared request state earns its cost by preserving leaf correlation and real SCS response status; the fixed counter array earns its cost by bounding warnings without dropping their totals. Keep response rendering, persistence, and logging emission separate; do not add a second family of response/logging helpers.

**Wire format:** `slog.NewJSONHandler`, minimum Info, `AddSource: false`; `ReplaceAttr` renames `slog.MessageKey` to `message` and lowercases `slog.LevelKey`. One newline-terminated JSON object per event. The `message` itself is the stable event name; do not duplicate it in a separate `event` field. Server operational output goes to **stdout**, avoiding Railway's stderr error classification. Durations are numeric milliseconds, never duration strings.

Event names: `request_failed`, `database_slow_summary`, `orm_warning`, `http_server_error`, `startup_started`, `server_listening`, `startup_failed`, `server_failed`, `shutdown_requested`, `shutdown_completed`, `shutdown_failed`, `session_cleanup_failed`.

**Safe error attributes:** `error_kind`, `error_type`, and optional `sqlstate`. Never serialize an error object, call `err.Error()` for an operational event, or log SQL, bind values, database messages/details/hints, table/constraint identifiers, URLs, query strings, cookies, headers wholesale, form values, names/usernames, passwords, tokens, or token digests. No student IDs/field values are needed: request ID plus operation/route/stage identifies the failure.

Implement classification with `errors.Is`/`errors.As`, not error-message matching:

1. `context.Canceled` → `canceled`; `context.DeadlineExceeded` or a timeout `net.Error` → `deadline_exceeded`.
2. Extract SQLSTATE through `interface { SQLState() string }`, preserving wrappers without importing pgconn. Accept only five uppercase ASCII letters/digits. Class `08` and `57P01`/`57P02`/`57P03` → `connection_failure`; `57014` → `query_canceled`; other valid SQLSTATE → `database`.
3. `sql.ErrTxDone` → `transaction_done`; `sql.ErrConnDone`, `driver.ErrBadConn`, `net.ErrClosed`, `io.ErrClosedPipe`, `syscall.EPIPE`/`ECONNRESET`/`ECONNREFUSED`, network errors, EOF/unexpected EOF → `connection_failure`.
4. Otherwise `unknown`. `error_type` is the concrete Go type of the matched error, or original error when no class matches; never its value. Nil produces no error attributes.

Request cancellation is Warn. Broken-pipe/reset/closed-connection errors during `stage=render` are also Warn; database connection resets, deadlines, and other unexpected failures remain Error. Lifecycle/setup/background failures remain Error. Expected validation/auth/not-found/conflict/rate-limit responses do not become warning events.

**Legacy HTTP diagnostic boundary:** `ServerErrorLog` returns `log.New(privateWriter, "", 0)`. Its writer consumes a whole stdlib log record and emits one `http_server_error` Error event, dropping raw bytes. Classify only fixed prefixes into `kind=panic`, `accept`, `response_header`, or `other`; never forward panic values, peer addresses, headers, or stack dumps. It returns `len(p), nil` like a successful logging writer. Preserve net/http's existing panic/connection-abort behavior; do not add recover-and-500 middleware. These connection/server diagnostics have no reliable request context and must not invent correlation fields.

**Acceptance:** structured severity and privacy checks below pass; no new dependencies. GORM's installed `NewSlogLogger` is not suitable: its Trace eagerly calls the SQL callback and emits SQL/error text (`logger/slog.go:58–92`).

### 2. Make request failure ownership explicit

**After step 1. Customer/management and auth slices may run in parallel; integration owner owns main wiring.** `Requests` is the outer wrapper around the existing origin-protection/root-mux stack:

```go
logging.Requests(log, http.NewCrossOriginProtection().Handler(mux))
```

It creates one request-scoped state shared through context copies. State holds start time, validated request ID, latest matched route/operation, first reported failure/stage, and actual response status; it is not a queue. Handlers do not launch asynchronous work using this mutable state. `WithOperation` supplies immutable operation context for startup/background GORM work; the DB logger reads request state when present, otherwise that operation context.

Initialize request state explicitly to operation `other`; a background context without an operation also resolves to `other`, not array index zero. Static assets and the root redirect need no operation wrapper.

- Reuse `X-Railway-Request-Id` only if one header value contains 1–128 ASCII characters from `[A-Za-z0-9._-]`; otherwise generate `crypto/rand.Text()`. Do not accept a client `X-Request-ID` fallback. Return the chosen ID as `X-Request-ID`; never overwrite Railway's own response header. Correlation IDs are diagnostic, not trusted identities.
- Track actual status with a small ResponseWriter wrapper. Delegate headers/body; implicit Write commits 200; informational 1xx other than 101 does not commit final status; first final status wins. Do not forward redundant final WriteHeader calls, including SCS's 500-followed-by-handler-status sequence. Implement `Unwrap() http.ResponseWriter` for ResponseController/SCS. No current application path uses websocket/push/legacy close-notifier APIs; do not build an optional-interface framework.
- `Operation` wraps each matched route handler, normalizes the static operation name, and updates the shared state's operation and `r.Pattern` **after that mux matched**. It introduces no request copy. For auth, put it outside SCS LoadAndSave so session-load failures have a label too. Store the whole mux pattern (including the registered method, if any); `method` separately records the actual request method, so HEAD is distinguishable from GET. If no operation wrapper captured a pattern, use the outer request's matched Pattern when available, otherwise `unmatched`; never substitute URL.Path. Log standard HTTP methods verbatim; other methods become `OTHER`.
- `RecordError` records only the first non-nil error and its static stage; it emits nothing and never changes the response. Template/body-write failures cannot replace an earlier database failure. It uses the shared leaf pattern/operation, not a stale SCS request copy. A later `Operation` call cannot change an already captured primary failure's metadata.
- After the complete handler/session stack returns normally, emit at most one `request_failed` event when a failure was recorded or final status is 5xx. Fields: `request_id`, `method`, `route`, `operation`, `stage`, `status`, `duration_ms`, safe error attributes. No explicit response write means normal implicit status 200. A bare 5xx uses `stage=response`, `error_kind=unknown`. Successful and expected 4xx requests emit nothing. Rendering failure after 200 keeps `status=200`; do not invent 500 or buffer/re-render responses. Let panic unwind unchanged to net/http and its safe ServerErrorLog; do not also emit an inferred request outcome while unwinding.
- `Requests` is required wiring for application handlers. Use a private context accessor with a clear internal invariant rather than silently dropping failures when middleware is absent. Migrate every application/test handler root in this step. No alternate fallback logger/path.

Route-operation contract (all literal, bounded):

| Registration / boundary | Operation |
| --- | --- |
| `GET /customer/{token}` including HEAD | `link_read` |
| `POST /customer/{token}` | `final_submit` |
| `POST /customer/{token}/draft` | `draft_save` |
| `GET /management/{$}` | `link_list` |
| `GET /management/trash` | `trash_list` |
| `POST /management/links` | `link_create` |
| `POST /management/links/{id}/cancel` | `link_cancel` |
| `POST /management/links/cancel` | `link_cancel_all` |
| `GET /management/settings` | `settings_load` |
| `POST /management/settings` | `settings_update` |
| `GET /management/students` | `student_list` |
| `GET /management/students/{id}` | `student_get` |
| `GET /management/students/{id}/fields/{field}` | `student_field_get` |
| `POST /management/students/{id}/fields/{field}` | `student_update` |
| `POST /management/login`, before LoadAndSave | `sign_in` |
| `POST /management/logout`, before LoadAndSave | `sign_out` |
| `auth.Manager.Require`, before LoadAndSave | `session_check` |

**Customer/management migrations:** in `internal/customer/customer.go`, `internal/management/management.go`, and `internal/management/students.go`, wrap registrations with `logging.Operation(name, handler)` and replace all 13 direct log calls with `RecordError`. Keep all response messages, statuses, validation order, HEAD behavior, fragments, and SQL calls unchanged.

- Customer `show` records `stage=render`; save branch `save`; lookup branch `lookup`. Draft/final distinction comes from route registration, including malformed-input fallback lookup paths.
- Management `execute` records `render`; render's List failure `list`; create's combined settings/create branch `create`; cancellation branches `cancel`/`cancel_all`; settings load/save `load`/`save`.
- Students `load` records `load`, list records `list`, UpdateField records `save`; leaf operation is set before load/validation. Remove the student-only `%T` logging comment/policy.
- Change management's local callback to `func(http.ResponseWriter, *http.Request, int, string, any)` and the `execute` parameter of `registerStudentRoutes` accordingly. Migrate its seven calls: management `render`, `showSettings`; students list, detail, full field page, `student-save`, `student-field`. `RegisterRoutes` signatures remain unchanged.

**Auth migrations:** in `internal/auth/http.go`, configure `sessions.ErrorFunc` to `RecordError(r, "session", err)` followed by the existing SCS response `http.Error(w, http.StatusText(500), 500)`. Replace direct logging in Require/logout/login/render with stages `authenticate`, `logout`, `login`, `render` respectively. Change render to `func (m *Manager) render(w http.ResponseWriter, r *http.Request, status int, page loginPage)`; migrate its five calls (Require and four login branches). Keep expected sign-in rejections silent and public routes outside SCS.

**Fixture roots:** wrap `auth/http_test.go:newApp`, `management/management_test.go:newApp`, its `TestStoreFailuresAreServerErrors` direct mux, and `customer/customer_test.go:newMux` with Requests. Change customer `newMux` return to `http.Handler` and `TestErrorResponsesKeepNameForm`'s map to `map[bool]http.Handler`; all other newMux consumers use ServeHTTP or httptest.NewServer and need no semantic change. Ordinary fixture loggers write to `io.Discard`; logging contract cases inject a private buffer and call ServeHTTP synchronously to avoid races with network-response timing. Do not change global loggers in parallel tests.

**Acceptance:** failures retain existing HTTP bodies/statuses, carry the correct operation/route, and emit once after SCS. No per-success access logging; no customer token or private values in events.

### 3. Bound database warnings and remove logging workarounds

**After step 1; can run alongside step 2 in its own files. Main integration waits for both.** Replace `main.go`'s `logger.New(...)` configuration with `logging.NewDBLogger(log)`. Keep threshold **strictly greater than 200 ms**, matching the current logger; emit `database_slow_summary` at Warn every **30 seconds**, once per nonempty operation bucket.

- Fixed operation allowlist: the 17 route/boundary names above plus `startup_migrate`, `session_cleanup`, `cli_user`, and `other` (21 buckets maximum). Declare it once as a private fixed string array. Normalize names to array indices with `slices.Index`, falling back to the `other` index; do this when constructing a route wrapper or background context, not on every Trace. Request/background contexts carry the index. Use a fixed array of `{count uint64; worst time.Duration}` buckets, not a map. Never key by request ID, URL, SQL, resource ID, error message, or caller-supplied dimensions.
- Successful slow Trace calls increment the indexed bucket under a mutex. Fast queries and `err != nil` return immediately; failed queries remain visible through their terminal HTTP/lifecycle/CLI owner, not the slow-success aggregate. Trace delegates its duration to private `func (l *DBLogger) observe(ctx context.Context, elapsed time.Duration, err error)` so exact boundary tests do not need sleep or a public clock interface.
- **Never invoke Trace's SQL callback**, even for errors/slow queries. Do not log SQL kind, rows affected, file/line, or SQL text. No SQL parsing, compaction, formatting, or bind expansion for this logger.
- `Flush()` copies and resets active counters under the mutex, releases it, then writes events. Reuse fixed bounded storage; no growing maps or per-request retained samples. Fields: `operation`, `count`, `worst_duration_ms`, `threshold_ms=200`. Empty flush emits nothing. Concurrent Trace/Flush preserves counts exactly once. Failed queries are not sampled or suppressed at their primary owner.
- Implement LogMode with an immutable level view sharing the same aggregate state (never copy a mutex). Silent/Error views do not count slow warnings; Warn/Info do. Info must not enable statement dumps. Reuse `logger.Discard` for non-Trace Info/Error methods: the reachable GORM initialization/SQL failures are returned and owned by callers. Override Warn with one safe `orm_warning` event, without format/data arguments, so non-query ORM warnings are not silently disabled.
- The existing `serve` select loop owns the 30-second ticker; add a loop/case rather than a summary goroutine. Stop ticker on exit. Flush after startup migration and once during final run cleanup after auth cleanup has stopped; startup-error/CLI exits also flush. There is no queue and no dependency on the next query arriving, so isolated slow queries are eventually reported.
- Tag AutoMigrate's existing timeout context `startup_migrate`; valid CLI DB commands `cli_user`; auth-owned expiry statements `session_cleanup` via WithOperation. Existing request-derived contexts already reach all stores; no store API or SQL changes are needed.
- Delete `.Session(&gorm.Session{Logger: logger.Discard})` and its privacy comment/import from `internal/students/students.go:UpdateField`. Student writes regain bounded slow visibility.
- In `internal/students/students_test.go:TestStorePropagatesDatabaseFailuresAfterFieldValidation`, delete the injected legacy SQL logger/buffer assertion and now-unused imports; keep rollback/error propagation and pre-SQL validation assertions. Replace that privacy coverage at the production logging/HTTP boundary below, not with another student-only silencing test.
- Set `Logger: logger.Discard` in `internal/testdb/testdb.go`'s GORM config so negative fixtures do not emit unparameterized default SQL. Privacy/summary contract tests explicitly attach the production DBLogger to their isolated DB session; do not mistake a discard-only fixture for privacy proof.

**Acceptance:** maximum 21 summary lines per scheduled flush regardless of successful slow-query count, plus explicit startup/final partial flushes. All slow counts/worst durations retained; failures have no summary-window delay and remain visible through callers. Zero SQL callback evaluations in this adapter. Measure line/byte/callback reduction; do not claim an unmeasured throughput gain.

### 4. Close lifecycle and session-cleanup logging gaps

**Auth portion may run alongside customer/management after step 1; main integration is last.**

**Auth-owned expiry cleanup:** change constructor to `func New(db *gorm.DB, secureCookies bool, log *slog.Logger) (*Manager, error)`. Call `postgresstore.NewWithCleanupInterval(sqlDB, 0)`; keep that store assigned to SCS, with its existing persistence semantics. Replace Manager's `store` and `closeOnce` fields with `stopCleanup context.CancelFunc` and `cleanupDone chan struct{}`; remove `sync` import. Start one owned worker only after construction can no longer fail. It uses a five-minute ticker, no initial immediate sweep, and `context.WithTimeout(workerContext, dbTimeout)` around the same expiry predicate `expiry < CURRENT_TIMESTAMP`, implemented through `m.db.WithContext(logging.WithOperation(ctx, "session_cleanup")).Where(...).Delete(&Session{})`.

Put a single sweep in `func (m *Manager) deleteExpiredSessions(ctx context.Context, log *slog.Logger)`: apply the five-second timeout, perform DELETE, report `session_cleanup_failed` with safe error fields on failure unless the worker's parent context was canceled. Ticker worker calls it synchronously, stops ticker and closes cleanupDone on exit. `Close()` cancels and waits on cleanupDone; CancelFunc and closed-channel receive already make repeated Close safe, so no sync.Once remains. Preserve auth cleanup-before-pool-close order. Direct SCS persistence SQL remains outside GORM slow summaries by the user's choice.

Constructor callers to migrate: `main.go:run`; `internal/auth/http_test.go:newApp`; `internal/management/management_test.go:newApp`. LSP references were checked; integration-tagged callers were additionally confirmed by literal search `auth\.New\(|\bNew\(db, (true|false)\)` over main/internal Go files.

**Process logging in main:** server mode creates `logging.New(os.Stdout)` before setup. Add root attributes from an explicit environment allowlist only: `service` = `RAILWAY_SERVICE_NAME` or `PaymentWebsite`; `environment` = `RAILWAY_ENVIRONMENT_NAME` or `local`; `version` = `RAILWAY_GIT_COMMIT_SHA` or `unknown`; optional `deployment_id` = `RAILWAY_DEPLOYMENT_ID` and `replica_id` = `RAILWAY_REPLICA_ID`. Never enumerate the environment. No build stamping or new config flags.

Keep account CLI help, username-list output, password prompts, usage/validation messages, exit codes, and full startup migration order as an intentional operator interface, not application events. Do not convert them to JSON or log the CLI arguments. For CLI runs, diagnostic JSON (including summaries) uses stderr, leaving stdout data/prompts unchanged; no HTTP startup/shutdown events. Preserve existing human-readable command errors rather than serializing them as application events. Startup database/configuration errors in either mode use only safe classification and a static stage, never raw DSN/driver text. Do not split or redesign account commands just to change logging.

Remove `log.Fatal` and pre-bind `Listening` output. Use `func run(log *slog.Logger) (err error)` and existing defer-based cleanup; main only exits nonzero after run returns and never prints/logs that returned error again. Preserve early CLI help handling. An invalid top-level CLI command is printed once as existing human text inside run before returning. After this argument handling, run owns setup failures through a named error result, static stage variable, and deferred safe classifier. Disable that defer when setup ownership ends: before calling auth.RunCLI or before handing the bound server to serve. If auth.RunCLI returns a command error, run prints it once to stderr as operator output, then returns it. Serve alone logs serving/shutdown failures. Stages: `configuration`, `database_open`, `database_pool`, `database_connect`, `database_migrate`, `auth_initialize`, `management_routes`, `customer_routes`, `listen`.

- `startup_started` Info before server setup.
- Bind `net.Listen("tcp", server.Addr)` in run; bind failure is `startup_failed`, stage `listen`, with no listening event. After successful bind, emit `server_listening` Info with actual `addr`.
- Change serve to `func serve(ctx context.Context, server *http.Server, listener net.Listener, log *slog.Logger, slow *logging.DBLogger) error`; use `server.Serve(listener)` in the existing result goroutine. Set `server.ErrorLog = logging.ServerErrorLog(log)`.
- On context cancellation during serving: `shutdown_requested` Info with `drain_timeout_ms=10000`; preserve the independent background 10-second Shutdown context and force-Close fallback. Never attach the signal context as request BaseContext.
- Successful drain: `shutdown_completed` Info with `duration_ms`. Failed drain/forced close: one `shutdown_failed` Error with elapsed duration and safe joined-error classification, then nonzero return; do not also claim completion or emit a generic duplicate fatal.
- Unexpected Serve error: `server_failed` Error, stage `serve`. Preserve http.ErrServerClosed handling and existing resource closure order. Stop auth worker before pool close; final slow flush runs after that worker stops.

PostgreSQL checkpoint LOG lines are emitted by the database service, not this application's logger. Leave PostgreSQL severity/configuration unchanged; application-error investigation must scope to the app service and structured Error events, not treat every database stderr line as an application failure.

**Acceptance:** no success event before bind, no duplicate fatal, observable graceful/forced shutdown outcomes, unchanged in-flight-request drain, quiet expected session cleanup, and no unstructured dependency expiry failures.

## Critical files and anchors

Line numbers are navigation hints; reread before editing.

- `main.go:61–88,107–163` — shared pool/migration, nested mux composition, startup/serve/drain integration owner.
- `internal/auth/http.go:37–94,108–129,220–227` — SCS configuration, request-copy boundary, cleanup lifecycle, render signature.
- `internal/customer/customer.go:37–79,98–153` — one shared handler serves GET/HEAD/final/draft; preserve invalid-input lookup/replay precedence.
- `internal/management/students.go:62–81,119–194` — field allowlisting/load precede input parsing; leaf operation must be attached before either.
- `internal/testdb/testdb.go:18–63` — schema isolation applies to GORM and direct SCS SQL; reuse without new fixture infrastructure.

## Verification

### Observed planning baseline

- `env GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOFLAGS=-mod=readonly go vet -tags=integration ./...` completed successfully with no output, using installed Go 1.26.9. Configured preferred toolchain remains 1.27.2; no toolchain file changed.
- gopls workspace diagnostics: no issues found. Staticcheck/golangci-lint/dead-code binaries were unavailable; no installs performed.
- Existing dev server: `curl --silent --show-error --head --max-time 10 http://127.0.0.1:3000/favicon.ico` returned HTTP 200, without a request-ID response header.
- Cached govulncheck v1.9.0 completed from its module directory: `env GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOFLAGS=-mod=readonly go run ./cmd/govulncheck -C /home/gabrielvelez/projects/PaymentWebsite ./...`. Result: zero affected vulnerabilities, zero in imported packages; four in required modules not called by this code. Initial version-qualified invocation was blocked by an offline deprecation-metadata lookup; running cached source avoided installation/module fetch. This scanned with Go 1.26.9, not the preferred deployment toolchain.
- No integration suite, database mutation, performance test, or Railway deployment was run during planning.

### Regression contracts to implement

Use standard testing/httptest and existing real PostgreSQL fixtures; no mock framework, permanent benchmark suite, or source-text assertions.

1. **`internal/logging/logging_test.go`:** feed wrapped cancellation/deadline/SQLSTATE/connection/unknown errors containing distinctive private marker strings; every serialized event is one JSON line with correct level/class and no marker. Feed a stdlib panic-style multiline record to ServerErrorLog; it yields one safe `http_server_error`, not its panic value/stack/address. Assert stable event/attribute semantics, not timestamps or incidental formatting.
2. **`internal/logging/http_test.go`:** actual nested mux plus WithContext copies; first error followed by render error emits one request event, leaf pattern remains correct, and final committed status wins over intended later status. Cover request-ID reuse/rejection and valid generated fallback (not a statistical randomness test), implicit 200, 1xx then final response, render failure after 200, and zero events for successful/expected 4xx requests. Assert forbidden token/URL/query/cookie/body values absent. Root middleware must retain origin protection and Unwrap behavior.
3. **`internal/logging/database_test.go`:** deterministic synthetic Trace/observe inputs and explicit Flush: fast, exactly-200ms boundary, successful slow, failed slow, unknown operation, multiple buckets, concurrent recording/flush, empty/final flush, and LogMode copies. Inject a callback that fails the test if invoked. Counts/worst values must be exact and race-safe; no sleeping 30 seconds or public clock/config abstraction.
4. **`internal/logging/http_integration_test.go` (`//go:build integration`, external `logging_test` package):** assemble actual auth/customer/management routes with a buffer-backed production logger and production DBLogger on a testdb.Open schema; drive handlers synchronously with httptest recorder. Reuse the existing failure mechanisms, not fake stores:
   - Rolled-back transaction: final POST and `/draft` produce exactly one Error each, `final_submit` versus `draft_save`, status 500, correct token-free route, request ID and elapsed field.
   - Schema-local check constraint rejecting `Reject completion`: final submit yields SQLSTATE `23514` once, no rejected/draft name or token in logs, no completion/student mutation; removing constraint allows the same submission to succeed without a success event. Existing `TestSubmissionFailureRollsBackAndAllowsRetry` remains the domain contract reference.
   - Student field constraint rejecting `Rejected by database`: authenticated field POST yields one `student_update`/`save` event, SQLSTATE `23514`, with original student/source unchanged and no values in logs. Missing students table covers a failure during the preliminary load while retaining `operation=student_update`.
   - Rename sessions table: forged staff cookie on public customer HEAD stays 404 and emits no error; protected management emits one safe `session_check`/`session` event with status 500 and route `/management/`. For commit failure, use a separate fresh schema with an empty sessions table and `ALTER TABLE sessions ADD CONSTRAINT reject_session_commit CHECK (false)`. Seed a valid low-cost test Argon2 account as existing auth fixtures do; POST correct credentials with no cookie. SCS Load does not query a missing token and RenewToken has no old token to delete, so the INSERT fails during redirect commit. Assert one `sign_in`/`session` event, SQLSTATE `23514`, actual status 500 rather than intended 303, and no username/password/token.
5. **`internal/auth/http_test.go`:** add `TestSessionCleanupDeletesOnlyExpired`: schema-local expired and future sessions; invoke deleteExpiredSessions directly, assert only expired rows removed. Rename sessions table and invoke again: exactly one safe cleanup error. Parent cancellation produces no cleanup-error event. Retain existing double-Close, session persistence, expiry, revocation, and public-bypass tests.
6. **`main_test.go`:** `TestServeLifecycle` uses a real ephemeral listener and handler coordination channels (not a second Air/full application instance). An in-flight handler completes after shutdown is requested → request succeeds and one completed event. A blocked handler exceeds the unchanged ten-second drain → force-close, one failure event, no completed/duplicate fatal. Verify no signal-triggered cancellation of in-flight request contexts. Tests close their listener/handler resources.

### Commands and runtime proof after implementation

Work from `/home/gabrielvelez/projects/PaymentWebsite`. Reuse the one existing Air server at port 3000; only if nothing listens may execution start Compose DB and `go tool air`. Never inspect `.env*`; existing Air loads its private environment. The README's dedicated local test database is `paymentwebsite_test`; never point integration tests at the app/shared database. If absent, create it once with `docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test` during execution, not planning.

```fish
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
go test -race ./internal/logging .
go test -race -tags=integration ./...
go vet -tags=integration ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Run focused tests during final verification, not repeated mid-flight checks across parallel edits. If the preferred toolchain cannot execute, use the already-proven local toolchain through `GOTOOLCHAIN=local` and report that distinction; do not alter go.mod/go.sum/toolchain to work around it.

**Before/after log-volume measurement:** run a throwaway Go program under ignored `tmp/` using the exact old logger configuration (Warn/200ms/parameterized) and the new DBLogger against identical 10,000 successful 250ms Trace calls with multiline synthetic SQL. Count physical lines, bytes, SQL callback evaluations, and new summary count/worst duration. Flush new logger explicitly; expect one summary for one operation, count 10,000, no SQL callback calls. Print the observed comparison; do not send load to Railway or claim that a synthetic log-volume result proves database latency improvement.

**Actual application smoke:** after Air rebuilds, HEAD `/favicon.ico` and a synthetic unusable `/customer/{token}` with `X-Railway-Request-Id: logging-smoke-001` must return 200/404 respectively, preserve existing headers, return `X-Request-ID: logging-smoke-001`, and produce no request-failure/access records. Exercise the synthetic summary program as a real Go program, not only tests. Run the built application once with a deliberately malformed **synthetic** DATABASE_URL (include a fake password marker) so startup fails before any listener/database mutation: one safe `startup_failed`, no marker, no `server_listening`, nonzero exit. Run `--help` and `user --help` and verify human CLI output remains unchanged and free of server lifecycle JSON. Use a separately named scratch binary, never overwrite Air's `tmp/main`.

Concrete smoke commands (Fish; fake password is only a privacy-test marker):

```fish
curl --silent --show-error --head -H 'X-Railway-Request-Id: logging-smoke-001' http://127.0.0.1:3000/favicon.ico
curl --silent --show-error --head -H 'X-Railway-Request-Id: logging-smoke-001' http://127.0.0.1:3000/customer/invalid-smoke-token
go build -o /tmp/paymentwebsite-logging-smoke .
env DATABASE_URL='postgres://smoke:PRIVATE-LOG-MARKER@localhost:badport/db' /tmp/paymentwebsite-logging-smoke
/tmp/paymentwebsite-logging-smoke --help
/tmp/paymentwebsite-logging-smoke user --help
go run ./tmp/logging-volume-smoke.go
```

The malformed-URL command is expected to exit nonzero; run it separately, not in a success-only command chain. The two help commands must not open the database. `logging-volume-smoke.go` is the throwaway measurement program specified above.

Verify forced/graceful shutdown via the real-listener lifecycle scenarios above; do not stop the user's dev server just to test shutdown. No push/deploy authorized. Railway severity parsing and native HTTP-log correlation can only be verified live after a separately approved deployment; do not claim that deployment proof from local JSON checks.

## Assumptions and contingencies

- User-selected boundaries are fixed: keep postgresstore persistence and existing CLI migration behavior; move only expiry ownership. No new session store or telemetry infrastructure.
- Local/non-Git deployments may lack commit/deployment metadata: emit `version=unknown`, omit absent IDs; never infer a release or add build tooling.
- A full operation allowlist/30-second summary is intentionally less granular than SQL text and can delay a single slow warning up to one interval. Failures are not aggregated, and final flush covers normal exit; SIGKILL cannot guarantee in-memory summary delivery. No queue/persistence is added to solve that ceiling.
- Direct SCS persistence queries remain outside GORM timing summaries. Their load/save failures are still visible through SCS ErrorFunc. Do not claim complete database latency instrumentation.
- Keep CLI command text as intentional operator output; the server operational privacy contract forbids raw errors. Do not turn all stderr/stdout into a global redaction stream.
- If runtime prerequisites cannot be made available without reading secrets, record the exact blocked runtime check while completing source and non-DB checks. Do not fabricate passes or silently remove acceptance scenarios.

## Evidence sources

- Railway JSON parsing, stdout/stderr severity, request-log fields, and 500-lines/second rate limit: https://docs.railway.com/observability/logs
- Railway correlation header: https://docs.railway.com/networking/public-networking/specs-and-limits
- Deployment/version variables: https://docs.railway.com/variables/reference
- GORM custom logger contract: https://gorm.io/docs/logger.html (also checked through Context7).
- Installed sources read: GORM v1.31.2 `logger/logger.go`, `logger/slog.go`, `callbacks.go`; SCS v2.9.0 `session.go`, `session_go120.go`; postgresstore v0.0.0-20251002162104-209de6e426de `postgresstore.go`; pgx v5.10.0 `pgconn/errors.go`; stdlib `net/http/server.go`.
