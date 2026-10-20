# Link lifecycle fixes — phased overview

## Context

Create a self-contained roadmap for four reported issues: cancellation losing newly created URLs, incorrect customer HEAD handling, unbounded retained history, and possible duplicate rows from overlapping create/cancel responses. The agreed scope uses two implementation phases; each gets its own separate detailed plan before code changes. This overview fixes phase goals, behavioral boundaries, risks, and acceptance criteria—not API signatures or patch mechanics. Approval accepts the roadmap, not immediate implementation of either phase.

## Vetted claims and evidence

| Reported issue | Finding |
| --- | --- |
| Cancel wipes newly created URLs | **Confirmed in source.** Both cancel handlers reload the registry through `Store.List`; the template replaces the entire list. Only `Create` returns a raw token, so unrelated URL/Copy controls disappear. `README.md:49` promises recopying until reload; line 15 describes the conflicting whole-registry cancel refresh. |
| HEAD returns 422 and marks Opened | **Confirmed, with qualifications.** An active unsubmitted link gets 422; an active submitted link gets 409. `Store.Open` writes `opened_at` before the GET-only branch. Changing that branch to non-POST fixes routing but not the state mutation. |
| History has no limit | **Confirmed.** `Store.List` loads all rows oldest-first, including canceled/expired records; cancellation retains rows and names. No automatic link-retention deletion path was found. Acceptability at small mock scale is a deliberate ceiling, not a benchmark result. |
| Create/cancel overlap duplicates a row | **Conditional source finding, not a reproduced ordinary-click bug.** A whole-list cancel snapshot can contain a committed creation before its delayed creation response appends the same row. However, the existing `hx-disabled-elt="button"` disables every button in the document during requests. Normal overlapping button clicks are already blocked. |

Primary anchors: `internal/management/management.go:54-122`; `internal/management/templates/index.html:21-65`; `internal/links/links.go:138-188,211-247`; `internal/customer/customer.go:70-110,146-148`. Line numbers are navigation hints; reread before each phase plan.

Go's installed `net/http/server.go:2504-2506` confirms GET patterns also match HEAD. The pinned [HTMX 2.0.11 source](https://github.com/bigskysoftware/htmx/blob/v2.0.11/src/htmx.js) confirms that `findAttributeTargets`/`querySelectorAllExt` resolve bare `button` against the document root, and `disableElements` disables those matches. This is UI gating, not a request-level mutex.

If overlapping same-page submissions bypass that gating, cancel-all can still collide with creation. Its database update and subsequent list query are separate: a later creation can survive; a delayed creation response for an already-canceled link can display stale active state and a revoked URL. That outcome is not merely cosmetic. Requests from another tab do not insert response fragments into this tab; cross-tab status freshness is a separate limitation.

## Approach — suggested phases

### Phase 1 — Preserve management-page link state

**Goal:** cancel one link without discarding any other newly created link's URL or Copy control. Covers reported items 1 and 4, plus the accepted limitation in item 3.

- Change single-link HTMX cancellation to replace only its own row, using the existing row rendering and cancellation behavior. Keep the canceled row, saved name, and staff attribution; do not reload the entire registry merely to render one cancellation.
- Keep cancel-all's full-list refresh and ordinary non-HTMX submissions' full-page fallback. The URL-preservation guarantee applies to the current HTMX page; full navigation/reload still loses one-time URLs.
- Preserve the existing page-wide button lock and verify it through actual UI interactions. **User selected normal-UI locking, not a new request serializer.** Do not add queues, abort/retry machinery, cross-tab synchronization, or promises about programmatic overlapping submissions.
- Preserve idempotent cancellation, expiry behavior, and error reporting: expired rows remain Expired, repeat cancellation does not rewrite prior attribution, and failures do not falsely show success or discard unrelated URLs.
- Record the deliberate unbounded-history choice with a `// ponytail:` comment at `Store.List`: acceptable for the current two-staff, small mock-data use; pagination is the upgrade path before sustained history growth. Do not implement pagination, arbitrary limits, or automatic deletion.

**Reuse and ownership:** management's existing `link` template, notices, `Store.Cancel`/`CancelAll`, and integration fixtures. Main work lives in `internal/management/management.go`, `internal/management/templates/index.html`, and their existing tests; the ceiling comment belongs in `internal/links/links.go`. Preserve `static/management.js` clipboard behavior and feedback ordering; change it only if the chosen row-update behavior demonstrably requires it.

**Accepted tradeoff:** unrelated customer statuses no longer refresh incidentally after a single cancellation; they refresh on normal full-page reload. Adding polling or a client-side token cache is not part of this fix. Raw tokens remain hash-only in PostgreSQL and are not persisted in browser storage.

**Exit:** the Phase 1 browser checks below pass, and the existing HTTP test that expects a full registry after single HTMX cancel is changed to protect the row-only behavior.

### Phase 2 — Make HEAD a non-mutating customer lookup

**Goal:** HEAD checks whether a customer link is usable without validating a submitted form or recording a customer visit. Covers reported item 2.

- Return HTTP 200 for usable links, including submitted/locked links; return 404 for unknown, malformed, canceled, or expired links. HEAD responses have no body. Lookup failures remain server errors, not invented 200/404 results.
- **User selected no Opened mutation from HEAD.** Reuse existing token validation and usable-link rules, but separate the HEAD lookup from the mutation currently bundled into `Store.Open`. The later phase plan chooses the smallest clear API shape; the one-line non-POST branch change alone does not meet this phase's goal.
- Preserve GET's first-visit tracking, POST autosave/final-submit behavior, submitted-form locking, and existing security/error headers. Unsupported methods remain unsupported.

**Reuse and ownership:** `internal/customer/customer.go`, existing `links` lookup scopes, and the existing customer/store integration fixtures. No equivalent non-mutating public lookup currently exists; any new store operation must reuse the same validity rules rather than duplicate them.

**Accepted limit:** GET-based previewers can still mark a link Opened. This phase does not attempt bot detection or redefine Opened as proof of human interaction.

**Exit:** HEAD returns the correct status without changing link state; a subsequent GET still records Opened; existing customer lifecycle checks remain green.

### Ordering and handoff

Plan and execute Phase 1 first because URL loss is irreversible under hash-only storage. Phase 2 is behaviorally independent and may be planned separately; both may touch `internal/links/links.go`, so integrate those edits deliberately. The duplicate-row concern belongs with management rendering, and the history comment is too small to warrant a standalone phase.

Each later session starts from this file, rereads its phase's anchors, and produces that phase's detailed implementation plan. It must retain the chosen scope and exit criteria rather than reopen settled policy by default.

## Assumptions and contingencies

- The operating ceiling remains two staff, mock data, and small retained history. If production use or sustained history growth becomes imminent, create a separate pagination plan before relying on the unbounded registry; do not quietly truncate or purge history.
- Normal-UI locking is the agreed concurrency boundary. If browser verification finds an ordinary user action bypasses it, fix that control-gating hole within Phase 1. Broader request serialization or concurrent bulk-update reconciliation requires a new scope decision, not an automatic expansion.
- Cancel-all revokes links usable when its update executes; it does not prevent later creations by another session. Other tabs remain independent. Keep this boundary when describing bulk cancellation.
- Recoverable-token storage and browser persistence are not acceptable substitutes for preserving untouched rows. No implementation should promise recovery after a real reload.

## Verification

**Planning evidence:** source inspection only. No app requests, tests, migrations, server starts, or working-tree changes were performed. The following checks belong to later implementation phases.

### Environment and existing checks

Run from `/home/gabrielvelez/projects/PaymentWebsite`. Use the existing single Air server at `http://127.0.0.1:3000`; never start a second. If nothing listens there, start the Compose database with `docker compose up --detach --wait --wait-timeout 90 db`, then `go tool air`. Local `.env` must supply `DATABASE_URL` and `SESSION_COOKIE_SECURE=false`; preserve existing configuration. Browser checks require a local signed-in staff session and disposable links.

Integration tests require a dedicated PostgreSQL test database. If absent, create it once with `docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test`. `internal/testdb/testdb.go:18-78` creates and drops an isolated schema per test; do not substitute the application database.

```fish
set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
# Phase 1
go test -race -tags=integration ./internal/management ./internal/links
# Phase 2
go test -race -tags=integration ./internal/customer ./internal/links
# After both phases integrate
go test -race -tags=integration ./...
```

Reuse behavioral checks rather than adding a new browser-test framework. `internal/management/management_test.go:300-388` currently asserts whole-list single cancellation at 353-360; change that contract instead of preserving it. `internal/customer/customer_test.go` already exercises autosave, submission locking, and expiry. Add focused permanent coverage for URL-preserving response behavior and HEAD-versus-GET state transitions; no wording tests for the ceiling comment.

### Phase 1 — actual browser proof

1. Create two links A and B without reloading. Cancel A. A remains as Canceled with its saved data/audit details; B retains the exact same usable URL and Copy control, with exactly one row for B. Copy B and compare the clipboard URL.
2. Exercise create and both cancellation controls with HTTP completion delayed. Other action buttons stay disabled while each request is pending; ordinary clicks cannot start overlapping mutations. After completion, controls recover and each created link appears once.
3. Delay clipboard completion after creation, then cancel another link. Late clipboard success/failure must not overwrite the newer cancellation notice. Verify the manual-copy fallback when clipboard access is unavailable.
4. Cancel all, then create another link after completion. Old usable URLs return 404; retained rows/names remain; the new link appears once and works. Confirm ordinary non-HTMX submissions still render usable complete pages and cancellation failures retain unrelated URLs.

### Phase 2 — HTTP and persisted-state proof

1. Create a fresh link without opening its customer URL. Send HEAD through an actual HTTP server, for example `curl --head "$fresh_url"` with `fresh_url` set to that URL: expect 200, no response body, and unchanged Not opened state with `opened_at` still unset.
2. GET that same URL: expect 200 and Opened. Repeated HEAD must not alter the first-open timestamp, saved name, or submission state.
3. HEAD a usable submitted link: expect 200 rather than 409; submitted state remains intact. HEAD unknown/malformed, canceled, and expired tokens: expect 404 without mutation. Exercise a lookup failure through the existing isolated failure-fixture pattern and retain a server-error response.
4. Exercise autosave and final submit, then rejected post-submit edits and revoked/expired access. Existing lifecycle behavior must remain intact. Verify body suppression through real HTTP rather than assuming an `httptest.ResponseRecorder` models transport behavior.

