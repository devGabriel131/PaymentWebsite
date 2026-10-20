# Link lifecycle fixes — Phase 1 implementation plan

## Goal and fixed scope

Implement Phase 1 of `LINK_LIFECYCLE_FIXES_PLAN.md`: canceling one link on the current HTMX management page must preserve every unrelated newly created URL and Copy control. Retain canceled rows, saved names, creator/canceler attribution, expiry rules, error handling, and ordinary full-page submissions. Phase 2 customer HEAD behavior is unchanged.

The existing document-wide `hx-disabled-elt="button"` is the concurrency boundary. No request serializer, queue, retries, abort machinery, cross-tab synchronization, polling, browser token storage, pagination, arbitrary history limit, or deletion job. Single cancellation no longer incidentally refreshes unrelated customer statuses. Reload/navigation and cancel-all still replace the full registry and lose one-time URLs. Cancel-all revokes links usable when its update executes, not creations afterward.

## Source and baseline

Read management handlers/template/script, the link store, management/link integration fixtures, the customer cancellation callsite, runtime bootstrap, and README. Current single cancellation calls `Store.List` and replaces `#generated-links`; only creation returns a raw token. Clipboard feedback already uses a request sequence, and Copy uses event delegation on the list.

Starting branch: `master`; the supplied overview is untracked user work. Preserve it. Existing server listens on port 3000; reuse it. Dedicated `paymentwebsite_test` exists. Baseline `go test -race -tags=integration ./internal/management ./internal/links` passes. Browser reproduction precedes implementation; observed evidence is recorded after execution, not assumed here.

## Contract

1. Change `Store.Cancel(ctx, id, staff)` to return `(*Entry, error)`. Keep staff validation and the existing usable-link update. Read back only the addressed record, including already-canceled or expired records, and convert through `Link.entry()`. Return no raw token. Malformed/unknown IDs remain successful no-ops, returning nil; database update/read failures return errors. Do not load the registry or invent cancellation state.
2. Existing records return their persisted state: active links become Canceled, already-canceled links keep original audit/timestamps, expired links stay Expired, and saved names/creator attribution survive. No schema or dependency changes.
3. Reuse management's current `link` and `result` templates for both created and canceled row responses. Rename the creation-specific response field to an operation-neutral name. HTMX create and single-cancel render exactly one addressed row plus the out-of-band notice, without `Store.List`. HTMX cancellation of absent/malformed IDs returns 204 with no swap. Ordinary forms retain complete-page rendering, including no-op cancellation; cancel-all retains complete-list fragments/pages.
4. Single-cancel forms target `closest li` with `hx-swap="outerHTML"`. Keep `hx-disabled-elt="button"`, confirmations, create append behavior, and cancel-all list targeting unchanged. No JavaScript change unless actual browser checks demonstrate a need.
5. Add a `// ponytail:` comment at `Store.List`: unbounded retained history is acceptable for two staff and small mock data; pagination is required before sustained growth. No comment wording tests.

## Parallel implementation units

- **Runtime/store owner:** `internal/links/links.go`, `internal/management/management.go`, `internal/management/templates/index.html`, `internal/links/links_test.go`, `internal/customer/customer_test.go`. Implement the contract, migrate every `Cancel` caller, strengthen existing store cancellation checks for returned states/errors, and add the history ceiling comment. Customer test changes are signature migration only, not Phase 2.
- **Management coverage/documentation owner:** `internal/management/management_test.go`, `README.md`. Replace the full-list HTMX cancellation expectation with the one-row contract; cover another existing row being excluded, retained names/audit, repeat cancellation, expired rows, and failed cancellation not returning a successful fragment. Retain full-page/cancel-all coverage. Document URL preservation and its navigation/bulk/status-freshness limits, page-wide normal-UI locking, and the history ceiling.

Both units use the contract above and edit disjoint files. Neither runs tests, builds, lint, or formatters. The orchestrator collects both, formats their union once, runs gates, and corrects any gaps before browser verification. No commit, push, or deployment is requested.

## Verification and exit criteria

### Automated checks

Use `TEST_DATABASE_URL=postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable`; isolated schemas come from `internal/testdb`.

- Format all changed Go files once; request LSP diagnostics for changed Go files (integration-tagged file limitations must be reported, not mistaken for a pass).
- Run `go test -race -tags=integration ./internal/management ./internal/links` after integration.
- After browser proof, run `go test -race -tags=integration ./...`, `go vet -tags=integration ./...`, and the README vulnerability gate. No server-side dependencies/toolchain/deploy config are changed.

### Actual browser proof

Use the single existing Air server, a disposable local CLI-created staff account, and disposable links. Do not expose/recover user passwords or persist raw URLs in browser storage.

1. Reproduce URL loss before editing. After fixing, create A and B without reload; save A's name through the customer flow; cancel A. A remains Canceled with saved name and audit details. B's exact URL and Copy control remain, its DOM row is untouched and appears once, clipboard matches B, and its customer URL returns 200.
2. Delay HTTP completion separately for create, single-cancel, and cancel-all. Use actual button clicks; every other action button stays disabled, attempted ordinary clicks send no overlapping mutation, controls recover afterward, and each newly created link appears once. Fix an ordinary-action gating hole if demonstrated; do not broaden concurrency policy.
3. Delay creation clipboard success, then failure, across cancellation of another row. Neither late result may overwrite the newer cancellation notice. Disable clipboard availability and exercise create/manual-copy fallback plus the row's Copy fallback.
4. Cancel all, then create after completion. Previously usable URLs return 404; retained names/history remain; the fresh row appears once and works. Exercise non-HTMX create/single-cancel/cancel-all as normal browser forms and require full usable pages. Exercise failed cancellation while an unrelated URL is present: no false success, no row/list replacement, URL unchanged, controls recover. Permanent isolated failure fixtures prove server-side 500 handling.

Exit only after all criteria pass or a concrete external prerequisite is exhausted and reported. Keep Phase 2 untouched, leave one Air server running, close owned browser tabs, and remove temporary verification scaffolding.

## Execution evidence

- Browser reproduction confirmed the original failure: canceling A removed B's URL and all Copy controls.
- After the fix, B retained the same DOM row, exactly one URL/Copy control, a matching real clipboard URL, and HTTP 200 customer access. A retained its saved name and creator/canceler details. A markup-bearing saved name rendered as text, not an element or script.
- Held create, single-cancel, and cancel-all requests each disabled every page button; ordinary clicks started exactly one mutation, and controls recovered. Each created row appeared once. No serializer or JavaScript change was needed.
- Delayed creation clipboard success and rejection both left the newer cancellation notice intact. Missing clipboard APIs produced both fallback notices; selecting the visible URL and native copying produced the exact URL in the real page clipboard.
- Bulk cancellation retained six rows and the saved name, removed displayed URLs, and made four previously usable URLs return 404. A subsequent creation appeared once and returned 200.
- With JavaScript disabled, actual create, single-cancel, and cancel-all form navigation returned HTTP 200 complete pages without HX-Request; rows/names remained, and a new post-bulk URL worked.
- An injected HTTP 500 proved browser error handling retained both target and unrelated DOM rows/URL and recovered controls; retrying against the real endpoint succeeded. Isolated PostgreSQL integration fixtures separately proved real server failures return 500 without successful row/list/notice fragments.
- A signed-out actual HTTP create returned 401 with HX-Redirect rather than exposing a fragment. Existing authorization/cross-origin, SQL-like ID, hash-only storage, audit, repeat-cancel timestamp, and expiry integration contracts are retained.
- Concurrent user edits introduced Lookup/HEAD-related code in shared store/customer files during this work. They were preserved, not authored as part of Phase 1. No commit, push, deployment, dependency, schema, or dev-server configuration change was made.
- Final combined-tree gates passed: race-enabled integration tests for management/links/customer and then `./...`, `go vet -tags=integration ./...`, and workspace Go diagnostics. `govulncheck` found no reachable vulnerabilities; it reported four module-level advisories outside called code.
- Owned browser tabs were closed and the disposable staff account removed. No repository verification scripts were created. The existing server remains listening on port 3000.
