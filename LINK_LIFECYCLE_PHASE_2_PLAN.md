# Link lifecycle fixes — Phase 2 implementation plan

## Scope and contract

Implement Phase 2 from `LINK_LIFECYCLE_FIXES_PLAN.md`: customer HEAD requests check usability without recording a visit, validating a name, or changing saved/submitted data. Usable links, including submitted/locked links, return 200. Unknown, malformed, canceled, and expired tokens return 404. Lookup failures return 500. Every HEAD response has no body.

GET continues recording the first visit. POST autosave/final submission, submitted-form locking, expiry/revocation, customer response headers, and unsupported methods keep their existing behavior. GET-based previewers can still record Opened; bot detection is out of scope. Phase 1 management rendering, concurrency, and history policy remain untouched.

## Current evidence

- `internal/links/links.go`: `Store.Open` validates through `tokenDigest`, queries through `active`/`usable`, then conditionally writes `opened_at`. `usable` uses the PostgreSQL clock and excludes canceled/expired links, not submitted links.
- `internal/customer/customer.go`: the GET route also handles HEAD. It currently calls `Open` and only treats GET as a page lookup, so HEAD reaches form validation or the submitted-form conflict branch.
- `internal/customer/customer_test.go`: isolated PostgreSQL schemas cover autosave, final submission, locking, expiry, and lookup errors using a rolled-back transaction. Existing HTTP checks use response recorders; HEAD body suppression needs a real HTTP transport.
- `internal/links/links_test.go`: token boundaries, progress, revocation, and expiry already have integration coverage.
- Initial branch: `master`; only the user-provided overview plan is untracked. The existing application is listening on port 3000. The dedicated `paymentwebsite_test` database exists.

## Implementation units

### A. Link store

Targets: `internal/links/links.go`, `internal/links/links_test.go`.

1. Add `Lookup(ctx context.Context, token string) (*Link, error)`, explicitly non-mutating. Move the existing validation/query portion of `Open` into it. Preserve nil/nil for invalid or unusable tokens and propagate database failures.
2. Make `Open` call `Lookup`; return early on error/no match. Preserve its conditional first-visit update through `active`, using the returned digest rather than hashing the token again.
3. Keep one validity policy: `tokenDigest` and `active`/`usable`. No new token parser, SQL predicate copy, migration, dependency, compatibility layer, or boolean-mode store API.
4. Extend existing store regressions only where needed to protect non-mutating lookup, usable submitted links, and unchanged Open behavior. Customer HTTP tests own the HEAD matrix; avoid duplicating it wholesale.

### B. Customer handler and HTTP regressions

Targets: `internal/customer/customer.go`, `internal/customer/customer_test.go`.

1. Select `store.Lookup` for HEAD before any call to `Open`; retain `Open` for GET/POST.
2. Treat a successful HEAD like a successful GET before submitted-lock or form-validation branches. Preserve the existing method registrations and five-second lookup context.
3. In the existing response-rendering closure, retain status and metadata but return before template execution for HEAD, including 404/500 paths. Keep `Cache-Control`, `Referrer-Policy`, `Vary`, HTML content type, and applicable HTMX refresh headers.
4. Add focused integration checks using a real `httptest.NewServer`, not a recorder, for HEAD transport behavior. Read persisted rows directly so assertions do not themselves record visits.
5. Protect fresh HEAD -> GET -> repeated HEAD state transitions, names/drafts, submitted/locked links, invalid/unknown/canceled/expired tokens, and the isolated rolled-back-transaction failure case. Assert 500 on lookup failure. Exercise unsupported methods and existing error/header behavior without changing those policies.

Units A and B edit disjoint files and may run together against the fixed `Lookup` signature and existing `Link` type. Subagents edit only; the orchestrator runs all formatters and gates once after both return.

## Verification and exit criteria

1. Before edits, exercise a disposable fresh token against the existing HTTP server and inspect `opened_at` to capture the current failure. Remove only the disposable fixture afterward.
2. Format the union of changed Go files once. Run LSP diagnostics and:

   ```fish
   set -gx TEST_DATABASE_URL 'postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable'
   go test -race -tags=integration ./internal/customer ./internal/links
   ```

3. Reuse the existing port-3000 server after Air rebuilds. With disposable rows, inspect raw HEAD responses and persisted state: fresh HEAD returns 200/no body and leaves Not opened/NULL `opened_at`; GET returns 200 and records Opened; later HEAD preserves the first-open timestamp, draft name, and submission state. Submitted HEAD returns 200; malformed/unknown/canceled/expired HEAD returns 404 without mutations.
4. Exercise lookup failure through the isolated failure fixture and real HTTP transport. Keep its 500 response and body suppression. Do not break the application database or replace the running development server to inject a failure.
5. Existing lifecycle checks must still prove autosave, final submit, rejected post-submit edits, revoked/expired access, and normal GET behavior. Verify response headers and 405 method rejection.
6. After smoke proof, update `README.md` with HEAD semantics and focused regression coverage; keep GET-previewer limitation explicit.
7. Final gates:

   ```fish
   go test -race -tags=integration ./...
   go vet -tags=integration ./...
   ```

Exit only after every check passes and all disposable smoke rows are removed. Keep the existing Air server running. No commit, push, deployment, Phase 1 implementation, pagination, bot detection, retry machinery, or cross-tab synchronization is requested.

## Execution evidence

- Before edits, the existing application returned 422 to a fresh HEAD and changed `opened_at` from NULL to set; the wire body was empty. The disposable baseline row was removed.
- After edits, raw HTTP against the same Air server proved fresh/repeated/draft/submitted HEAD returns 200 with zero body bytes and no persisted-row changes. GET recorded the first visit. Autosave and final submit worked; post-submit edits returned 409 without changing the row.
- Canceled, expired, mid-form-expired, unknown, and malformed HEAD returned 404 with zero body bytes. Revoked/expired GET or POST paths exercised retained the expected 404 behavior and saved data. PUT/DELETE on the page and HEAD on the draft endpoint remained 405.
- The integration failure fixture exercised HEAD lookup errors over a real HTTP connection: 500, no response body, preserved customer headers, and unchanged persisted rows. Both ordinary and HTMX HEAD variants passed.
- Passed targeted customer/links integration tests and the full integration suite with `-race -count=1`; `go vet -tags=integration ./...` and LSP diagnostics were clean.
- `README.md` now documents HEAD and the GET-previewer limitation. All three application smoke rows were removed; the existing Air server was retained. Concurrent Phase 1 changes were preserved, including the current `Cancel` return signature.
