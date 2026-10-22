# Enrollment error-path fixes

## Context

Fix the confirmed management-link feedback, agreement retry, payment-plan feedback, and redundant draft-write issues. Current `static/management.js:51–52` initializes `receiveLink` unconditionally, so missing Clipboard API does not require an optional resolver call. Investigation executed the existing management script in an isolated Chromium DOM: absent clipboard caused no exception, but a blocked list refresh left zero visible links while displaying “copy the link below.” Go findings are source-confirmed, not yet runtime-reproduced.

## Approach

### 1. Defer creation feedback until its registry row is visible

Edit only `static/management.js` for this behavior. Reuse `requestSequence`, `startAction`, `confirmCopy`, the existing `copied` promise, and HTMX's existing row refresh. A bare visibility guard would discard normal manual-copy feedback when clipboard completion precedes the asynchronous row swap; retain one pending result instead.

- Add module-local `let pendingCreationFeedback = null`, containing only `{ link, success }`, and `function showCreationFeedback()`.
- The helper returns if no pending result exists. If its anchor is detached, clear the result. Otherwise require `link.closest("li")?.parentElement === links` and `link.getClientRects().length > 0` before showing anything; a connected anchor under hidden `#link-tokens` must fail.
- On a visible row, clear the pending result and reuse existing output: `confirmCopy()` for success; otherwise `status.textContent = "Link created. Clipboard unavailable; copy the link below."`. Clear before emitting so later swaps cannot repeat the toast.
- Register one persistent `links.addEventListener("htmx:afterSwap", showCreationFeedback)`. No per-creation listeners, polling, timers, MutationObserver, or refresh retries are needed. HTMX emits this bubbling event after swapped content enters the DOM.
- Clear `pendingCreationFeedback` in `startAction`, alongside clearing the toast. Background list requests must continue bypassing `startAction`.
- In creation's existing successful `htmx:afterRequest` callback, keep URL normalization and unconditional `receiveLink(link.href)`. After awaiting `copied`, reject stale `sequence !== requestSequence`; otherwise store `{ link, success }` and call `showCreationFeedback()` immediately. This handles both completion orders: swap first or clipboard first.
- Leave `receiveLink` initialization and failure resolution unchanged: it is already always defined. Preserve manual row-copy behavior and all existing error strings. While no row is visible, creation's OOB “Link created.” or the refresh error remains; pending copy feedback cannot overwrite it. A later successful row refresh may then show current feedback.

`#link-tokens` and `hx-preserve` remain unchanged. No equivalent pending-feedback helper exists in the current script.

### 2. Load trusted agreement data before acceptance can fail

Edit `registerAgreementRoutes` in `internal/customer/agreement.go`; keep the shared five-second context and the store's acceptance API.

1. After copying the persisted link fields into `data`, but before parsing/processing the POST, prepare unaccepted agreement data from `configuration.Current(ctx)`. Accepted agreement views continue using their immutable snapshot without reading settings; payment views do not load agreement settings.
2. Move the existing settings-read/error block into a local `loadAgreementDate := func() bool`, capturing `ctx`, `data`, `r`, and `show`. Success sets `data.ProgramStartDate` from settings and returns true. Failure clears both date fields, records stage `"load"`, sets `"No pudimos cargar el acuerdo. Intenta de nuevo."`, renders 500, and returns false. Invoke it initially only for `view == "agreement" && data.TermsAcceptedAt == nil`.
3. Keep `AcceptTerms(ctx, token, r.PostForm.Get("program_start_date"))`: the POSTed date still participates in the store's atomic comparison; preloading must not substitute a new date into the acceptance attempt.
4. For generic acceptance errors, retain stage `"save"`, status 500, and `"No pudimos guardar tu aceptación. Intenta de nuevo."`; continue rendering from the already-loaded settings without any further database call.
5. Only `links.ErrProgramStartChanged` reloads settings after the attempted write, using the same local helper. Keep its existing 422 message and unchecked consent. This second read handles a staff change after the initial settings snapshot; if reload itself fails, clear the date and render the load-error 500 rather than offer a stale agreement.
6. Remove the old unconditional post-save settings read. Keep shared date parsing/Spanish labeling at the existing final rendering point. Invalid/empty dates still clear `ProgramStartDate` and withhold consent; do not let that missing-date message overwrite an existing 500 save error. Preserve the current missing-date message and POST-422 rule for other outcomes.

This adds one settings SELECT before successful acceptance, required to retain a server-trusted retry date if persistence fails. Do not render an agreement from unverified POST input or change `logging.RecordError`, which already preserves the first recorded failure. The local loader replaces the old block and serves its two real callsites; no new service abstraction.

### 3. Distinguish previous saved plan from rejected attempt

Edit only the existing `.SelectedPlan` notice in `internal/customer/templates/payments.html`. Keep `data.SelectedPlan` derived from persisted `link.PaymentPlan` and keep attempted `data.PaymentPlan` controlling the radio.

- Within `{{with .SelectedPlan}}`, branch on `{{if or $.Error (index $.FieldErrors "payment_plan")}}`; root qualification is necessary because `with` changes the template dot.
- Error branch, keeping `.payment-saved` and `role="status"`:
  `Plan guardado anteriormente: <strong>{{.}}</strong>. Tu intento de cambio no se guardó. No se ha realizado ningún cargo.`
- No-error branch retains the current full saved notice unchanged.
- With no previously saved plan, `.SelectedPlan` stays empty and no saved notice appears, including failed first saves. Duplicate/missing/invalid inputs and persistence errors keep their existing status, error, and attempted-selection behavior.

No new page-data field, plan API, or alternate template is needed.

### 4. Skip identical draft updates without weakening write outcomes

Edit `internal/links/links.go`. Reuse the allowlisted `students.FindField`, existing normalization at `internal/customer/customer.go:120–125`, `active`, and `saveResult`. Follow `ChoosePlan`'s guarded inequality pattern, not `students.UpdateField`, which also currently writes unchanged values and is outside this fix.

1. Add `.Where(clause.Neq{Column: clause.Column{Name: field.Key}, Value: value})` to `SaveDraft`'s UPDATE, retaining the usable-token and `submitted_at IS NULL` predicates. The installed GORM clause quotes the column and binds the value. All four `students.Details` columns are non-null strings, so ordinary inequality handles empty clears as well as populated values.
2. Change the private helper signature to:
   `func (s *Store) saveResult(ctx context.Context, digest []byte, result *gorm.DB, unfinishedResult SaveResult) (SaveResult, error)`.
   References were resolved: the only callers are `SaveDraft` and `Submit`. Pass `Saved` from `SaveDraft` and `Unavailable` from `Submit`; retain `Submit`'s atomic SQL unchanged.
3. Preserve the helper's initial error check and one-row committed-write branch, including its sole `s.changed()` call. In the existing zero-write fallback, SELECT current usable state as today: query error → `Unavailable, err`; absent usable row → `Unavailable, nil`; non-nil `SubmittedAt` → `AlreadySubmitted, nil`; active unfinished row → `unfinishedResult, nil`. The fallback must never broadcast.
4. Preserve unknown-field rejection before SQL, malformed-token handling, field isolation, and existing context-error propagation. Do not add HTTP preflight reads, caching, value normalization in the store, or a new exported outcome. Identical accepted drafts still return HTTP 200/“Guardado.”.

The extra enum argument keeps no-op draft success explicit without letting a zero-write final submission falsely claim success. Existing outcome classification remains in one helper.

## Critical files & anchors

- `internal/management/templates/index.html:63–68,129–145`: hidden token holder, preserved row slots, and creation OOB status; keep this handoff intact.
- `internal/customer/agreement_test.go:359–411`: missing settings must withhold consent; `newAgreementFixture`, `agreementForm`, and `agreementDate = "2027-02-28"` provide the regression fixture.
- `internal/links/links_test.go:1453–1527,1729–1761`: copy the existing observed PostgreSQL lock-wait/cancellation cleanup pattern, plus `testStore`, `digestOf`, and `studentsFor`.
- `internal/testdb/testdb.go:19–65`: existing real-PostgreSQL, per-test isolated-schema setup. No mock store/interface or new test dependency.

## Verification

### Prerequisites and commands

Work from `/home/gabrielvelez/projects/PaymentWebsite`. Execution may mutate only local mock/test data. Keep the existing Air listener on `127.0.0.1:3000`; if absent, start Compose DB with `docker compose up --detach --wait db`, ensure existing `.env` supplies `DATABASE_URL` and `SESSION_COOKIE_SECURE=false`, then start one `go tool air`.

Use the dedicated test database, not the application's database:

```fish
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
```

If that database does not exist, create it once with `docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test`. The test user needs schema-create/drop permissions. Keep an already-configured dedicated `TEST_DATABASE_URL` instead if one is present.

Add the three focused regressions below before their production edits, then run this targeted command once. Acceptance retry and identical-draft cases must expose the current bugs; the payment test protects existing saved/attempted-state separation and may already pass. Its copy clarification is verified on the actual browser surface.

```fish
go test -count=1 -tags=integration ./internal/customer ./internal/links -run 'Test(AgreementAcceptanceCancellationKeepsRetryForm|PaymentChoiceFailurePreservesSavedAndAttemptedState|DraftNoOpPreservesSaveOutcomes)$'
```

After all changes, run `go test -count=1 -race -tags=integration ./...` once, covering the new tests plus existing submission/cancellation races, missing/stale agreement dates, accepted replays, settings failures, and first-error logging. Do not weaken existing missing-settings/no-invented-consent tests.

### Permanent regressions

1. **Agreement retry — `internal/customer/agreement_test.go`, `TestAgreementAcceptanceCancellationKeepsRetryForm`.** Reuse `newAgreementFixture`. Hold `SELECT id FROM settings WHERE id = 1 FOR UPDATE` in a separate transaction; record its backend PID. Serve a valid agreement POST with a cancellable request context in a goroutine, through the fixture's logging-wrapped handler and an `httptest.ResponseRecorder`. Reuse the observed-lock pattern from `TestAcceptTermsObservesConcurrentStartDateChange`: poll `pg_stat_activity`/`pg_blocking_pids` with a bounded context until acceptance is blocked, then cancel the request. No arbitrary sleep and no GORM callback mocks. Require 500, no redirect, the save-specific alert rather than the load-specific alert, the configured hidden date, required unchecked checkbox, and enabled submit button. Verify link acceptance/date and retained student records are unchanged. Roll back the lock and submit the rendered retry date with a fresh request: require normal advancement to `/payments`. Defer rollback/cancel and drain the goroutine on every exit. Deterministic cancellation guards the failed-context regression; the live smoke below covers the actual five-second timeout.
2. **Saved versus attempted choice — same file, `TestPaymentChoiceFailurePreservesSavedAndAttemptedState`.** Use named subtests with a fixture that is accepted and saved as `full`. Exercise duplicate body `payment_plan=three&payment_plan=six` → 400/three checked; `payment_plan=other` → 422/no radio checked; and valid `payment_plan=six` under isolated constraint `CHECK (payment_plan = 'full')` → 500/six checked. Each response retains the prior full plan in the saved-state notice, the attempt/error, and the usable form; persisted plan remains `full` and other link/student fields stay unchanged. Assert semantic plan/radio/error state, not the complete Spanish paragraph. Verify new “previously saved / attempt not saved” wording in the browser smoke instead of adding a copy-snapshot test.
3. **No-op outcomes — `internal/links/links_test.go`, `TestDraftNoOpPreservesSaveOutcomes`.** Reuse `testStore` with a counted `changed` callback. Parse `7872345678` and `+1 (787) 234-5678` through `students.ParsePhone(raw, false)`; both produce `+17872345678`. First save returns `Saved` and announces once; equivalent normalized replay returns `Saved` with no additional callback. Capture `SELECT xmin::text FROM links WHERE token_hash = ?` around replay to prove no row UPDATE, not merely hidden notification. Cover explicit empty→empty no-op and populated→empty real clear. Named state cases must preserve `AlreadySubmitted` for an identical submitted draft, `Unavailable` for identical canceled/expired drafts and unknown/malformed tokens, and `Unavailable` plus error for a canceled-context call. Callback counts never increase on no-op/refused/error outcomes. Keep final-submission behavior covered by the unchanged existing suite.

### Actual application/browser smoke

Use browser tools, not a new browser-test framework. Reuse an authenticated local management session; if unavailable, create a uniquely named local smoke account using the existing interactive `go run . user add enrollment-smoke-<unique-suffix>` CLI and sign in at `/management/login`. Supply a generated password at its two hidden prompts; do not reset existing accounts. Use an existing configured program date; only if missing, set `2027-02-28` through the management date form.

- **Clipboard absent, refresh working:** disable `navigator.clipboard` with a browser init script before loading management. Click **Create & copy link** through real HTMX. Require no JS errors, no success toast, the absolute URL visibly on the created row, and the existing manual-copy status after that row arrives. Run again with row refresh deliberately delayed; feedback must wait, then appear exactly once.
- **Refresh blocked/failing:** allow creation POST but intercept only HTMX `GET /management/` refreshes (leave initial full-page GET alone). With clipboard absent, require no “copy the link below” while token remains hidden. Return 500 text `Could not load links. Please try again.` and ensure it remains through delayed clipboard completion. Then restore refresh and trigger the existing `management-changed` event: URL becomes visible and current pending feedback appears once. Repeat with a controlled successful clipboard promise; no toast before the row appears. Start a newer staff action before resolving an older promise and verify older feedback stays suppressed. Also check failed creation resolves without exception or toast.
- **Real acceptance timeout and retry:** create a fresh local link; submit valid contact data (`Prueba Inscripción`, matching `smoke@example.com` emails, phone `7872345678`, municipality `San Juan`) and read the displayed agreement date. Use a throwaway Bun/Node `net` client to send `POST /customer/<token>/agreement` headers with `Content-Type: application/x-www-form-urlencoded`, accurate `Content-Length`, `Expect: 100-continue`, and `Connection: close`, but withhold body. On the server's `100 Continue`—sent only when form parsing starts, after lookup and the new settings preload—wait 5.25 seconds, then send URL-encoded `accept_terms=yes&program_start_date=<displayed-date>`. This exceeds the handler's five-second context but remains below the ten-second server write timeout. Require final 500, no redirect, original save-specific alert, same date, and unchecked usable consent form. Display returned HTML in the existing same-origin browser tab to inspect it, then check consent and submit normally; require `/payments`. Do not shorten production timeouts or introduce an application test hook.
- **Failed plan change:** on that accepted token, save `full`, then submit duplicate `payment_plan=three&payment_plan=six` through a native form (an extra temporary hidden field can produce the duplicate). Observe 400, three checked, and “Plan guardado anteriormente: Pago Único. Tu intento de cambio no se guardó.” alongside the error. Reload `/payments`: full remains checked and ordinary saved notice returns.
- **Normalized autosave:** use a separate unfinished link and keep management open. Save phone `7872345678`, wait for its first registry refresh, then change the input to `+1 (787) 234-5678` and blur. Both requests return 200/“Guardado.”; the second emits no management refresh during a one-second observation window. Change to a distinct valid phone `2025550123` afterward and require a refresh, proving the live feed is still connected. The deterministic store regression supplies the stronger no-notification/no-write proof.

Inspect actual rendered agreement/payment error surfaces, record browser/network results, and close owned tabs. No deploy or push is part of verification.

## Assumptions & contingencies

- On a failed payment change, retain attempted radio input and explicitly label prior saved choice; do not suppress the banner.
- Keep token presentation dependent on the existing registry refresh. A blocked refresh does not expose the hidden one-time URL elsewhere; it must not claim a visible manual-copy fallback. Recovering the refresh completes pending feedback; a newer staff action invalidates it.
- No schema, dependency, deployment, or exported API changes. Keep the running Air instance; start one only if port 3000 has no listener.

