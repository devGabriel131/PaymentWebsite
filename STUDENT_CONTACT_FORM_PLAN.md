# Student contact form

## Context
Expand the existing customer name form into student full name, email, email confirmation, phone, and a Puerto Rican municipality or outside-Puerto-Rico selection. Persist the collected student details, display them in Students, and show only name and phone as customer information in the link registry. Keep the existing mobile-first welcome, including the removal of “¡Vamos pa’lante!”.

## Product decisions
- One required full-name field, minimum two words; letters, combining accents, ñ, and spaces allowed. Numbers, apostrophes, hyphens, markup, and emoji rejected.
- Required email plus required confirmation: syntax and matching only, no outgoing verification message, DNS/SMTP probes, or claims of mailbox ownership. Confirmation is transient, never persisted or shown in Students; a reloaded form requires it again.
- Required phone accepts valid Puerto Rico and United States numbers only. Residence selection never forces a PR phone: a student in a PR municipality can have a valid US number.
- Municipality dropdown contains the 78 municipalities in alphabetical order plus “Fuera de Puerto Rico”: 79 selectable values, excluding the empty prompt. No country/address follow-up field.
- Link registry shows only name and phone as personal information. Preserve status, dates, audit information, one-time URL, copy, and cancellation controls. Shared main/Trash renderer follows the same rule.
- Keep current submission lifecycle: this form submission creates the retained Students row and locks the link; agreements/payment are not implemented by this change. Customer labels/errors become Spanish; management labels remain English.
- Keep the welcome copy unchanged, including removal of “¡Vamos pa’lante!”.

## Verified starting point
- `internal/customer/customer.go` currently serves welcome at `GET /customer/{token}`, form at `GET /customer/{token}/forms`, final submission at `POST /customer/{token}`, and blur autosave at `POST /customer/{token}/draft`. Both GET routes use the same link-usability path; HEAD does not record a visit. Final submission is still a name-only operation.
- `internal/customer/templates/index.html` has the original logo and welcome copy without the removed closing phrase. The form and confirmation remain English. Its one `name` input posts on `change`, queues requests on that input, and swaps only `#save-status`; final POST swaps `#customer-content`.
- `pageData` currently carries only `Path`, `Name`, `Error`, `Submitted`, and `Welcome`. Completion clears the name from response data.
- `go.mod` specifies Go 1.27.3, GORM v1.31.2, PostgreSQL driver v1.6.3, and an indirect `golang.org/x/text v0.41.0`; no phone-validation package is present. HTMX is loaded by template at v2.0.11; the welcome loads no JavaScript.
- Prior browser/suite claims are historical, not fresh verification. This planning phase is read-only: no test execution, schema changes, or browser visits that would mutate link state.

## Approach

Implement as one clean cross-package cutover: shared types/parsers first, then persistence and customer request/UI contracts together, then management presentation. Migrate callers and their behavior tests with each signature change; do not leave name-only adapters or the old draft endpoint. Management presentation can proceed independently of customer layout once the shared fields exist.

### 1. Define one student-details type and shared field rules

In `internal/students/students.go`, add the concrete type:

```go
type Details struct {
    Name         string `gorm:"not null;default:''"`
    Email        string `gorm:"not null;default:''"`
    Phone        string `gorm:"not null;default:''"`
    Municipality string `gorm:"not null;default:''"`
}
```

Embed `Details` in `Student` and `students.Details` in `links.Link`, replacing their separate `Name` declarations. Preserve the existing `name` column and all source-hash, ID, timestamp, audit, and foreign-key definitions. New SQL columns are `email`, `phone`, and `municipality`, all text/non-null/default empty string. This is an additive `AutoMigrate` change using the existing call in `main.go`; no migration framework, automatic reset, or data backfill. Empty defaults support unfinished drafts and let old disposable mock rows open without invented contact data. Future submissions must pass all required validation.

Extend the existing ordered `students.Field` registry in `internal/students/fields.go`; keep `Value func(Student) string`, `Required`, and existing registry accessors. Change `Parse` from `func(string) (any, error)` to `func(raw string, required bool) (string, error)`, add `Options []string`, and define fields in this order:

| Key / column | Management label | Input type | Options |
|---|---|---|---|
| `name` | `Name` | `text` | none |
| `email` | `Email` | `email` | none |
| `phone` | `Phone` | `tel` | none |
| `municipality` | `Municipality` | unused for select | shared municipality choices |

All registry fields are required for final submission and canonical editing. Keep `ParseName(raw string, required bool) (string, error)`; add `ParseEmail`, `ParsePhone`, and `ParseMunicipality` with the same signature. Assign these functions directly to metadata. Keep `ValidationError` and return static, value-free English messages for management; never return raw library errors containing submitted data. Update `Store.UpdateField` to call `field.Parse(raw, field.Required)`; its public signature and single-column update remain unchanged.

Validation contracts:

| Field | Normalize and accept | Reject |
|---|---|---|
| Name | Validate UTF-8; trim surrounding whitespace; collapse internal Unicode space separators to one ASCII space. Allow Unicode letters and combining marks attached to a preceding letter in the same word. Preserve case and accents. Require at least two words on final submission/canonical edits; a one-word draft is allowed. | Digits, punctuation including apostrophes/hyphens, emoji, internal control characters, invalid UTF-8/NUL, or a word beginning with a combining mark. Do not silently remove bad characters. |
| Email | Trim; require one bare ASCII mailbox, at most 254 bytes and local part at most 64 bytes. Use `net/mail.ParseAddress`, require empty display name and parsed address equal to the trimmed input. Require a dotted DNS-style domain: nonempty labels of at most 63 ASCII letters/digits/hyphens, no leading/trailing hyphen. Lowercase only the domain; preserve local-part case, plus tags, and dots. | Display names, lists, comments, quoted local parts, address literals, whitespace/control characters inside the address, Unicode mailbox text, missing domain dot, malformed labels. No DNS/MX/SMTP calls and no provider-specific typo guessing. Punycode domains are accepted as ASCII labels. |
| Phone | Trim; allow ASCII digits, spaces, `(`, `)`, `-`, `.`, and one optional leading `+`. Parse with `phonenumbers.Parse(raw, "PR")`; resolve `GetRegionCodeForNumber`; require region `PR` or `US` and `IsValidNumberForRegion(number, region)`. Store and display `Format(number, phonenumbers.E164)`. Accept local ten-digit, leading-1, and +1 formats. | Letters/vanity numbers, extensions, short/incomplete or metadata-invalid numbers, Canada and other NANP regions despite +1, and other countries. No SMS verification, carrier lookup, or network call. |
| Municipality | Trim; exact membership in the shared 79-value list below. Stored value equals displayed Spanish option text, including `Fuera de Puerto Rico`. | Empty final selection, arbitrary submitted values, alternate spelling/casing not in the list. |

For every parser, `required=false` permits an empty value to clear a draft; nonempty email/phone/municipality must still be valid. `required=true` rejects empty values. Invalid edits never replace saved values.

Pin `github.com/nyaruka/phonenumbers/v2@v2.0.15` in `go.mod`/`go.sum` using `go get` during implementation. This release requires Go 1.25.0, below the existing Go 1.27.3, and uses the existing x/text/protobuf versions. Use only the root phone package; do not import carrier/geocoding/timezone. Standard-library `unicode`, `strings`, `unicode/utf8`, and `net/mail` cover the other parsers. A hand-written ten-digit/787/939 regex is insufficient for the agreed US-region policy; a general validation framework adds no value here.

Create `internal/students/municipalities.go` for the static choice list, `MunicipalityOptions() []string`, and `ParseMunicipality`. No municipality data source exists in the repo; one package-owned read-only list serves customer options, staff editor options, and server membership validation. No runtime API fetch or SQL lookup table.

Use these exact 79 values in this order; the empty UI prompt is not a value:

```text
Adjuntas
Aguada
Aguadilla
Aguas Buenas
Aibonito
Añasco
Arecibo
Arroyo
Barceloneta
Barranquitas
Bayamón
Cabo Rojo
Caguas
Camuy
Canóvanas
Carolina
Cataño
Cayey
Ceiba
Ciales
Cidra
Coamo
Comerío
Corozal
Culebra
Dorado
Fajardo
Florida
Guánica
Guayama
Guayanilla
Guaynabo
Gurabo
Hatillo
Hormigueros
Humacao
Isabela
Jayuya
Juana Díaz
Juncos
Lajas
Lares
Las Marías
Las Piedras
Loíza
Luquillo
Manatí
Maricao
Maunabo
Mayagüez
Moca
Morovis
Naguabo
Naranjito
Orocovis
Patillas
Peñuelas
Ponce
Quebradillas
Rincón
Río Grande
Sabana Grande
Salinas
San Germán
San Juan
San Lorenzo
San Sebastián
Santa Isabel
Toa Alta
Toa Baja
Trujillo Alto
Utuado
Vega Alta
Vega Baja
Vieques
Villalba
Yabucoa
Yauco
Fuera de Puerto Rico
```

### 2. Preserve field-isolated drafts and atomic final submission

Change link-store methods in `internal/links/links.go` to:

```go
func (s *Store) SaveDraft(ctx context.Context, token, key, value string) (SaveResult, error)
func (s *Store) Submit(ctx context.Context, token string, details students.Details) (SaveResult, error)
```

`SaveDraft` resolves `key` through `students.FindField` and rejects unknown keys with `students.ErrUnknownField` before SQL. For a known key, update only `field.Key` with the validated string value, preserving explicit empty writes and the existing active/unfinished predicates. Do not use a struct update that skips zero values, or save an entire form snapshot on blur. The customer boundary owns parsing as before.

Extend all four parts of the existing completion CTE together: UPDATE sets `name,email,phone,municipality`; RETURNING includes all four; INSERT names all four student columns; SELECT copies all four from the winning update. Keep timestamps, 168-hour expiry, guards, `saveResult`, and committed-write notifications. A failed INSERT must roll back every submitted field; a replay or racing draft cannot alter completion. Never split source update and enrollment into separate SQL statements.

Add only `Phone string` to `links.Entry` and populate it in `Link.entry`. Change In progress detection to any of the four stored detail fields being nonempty. Clearing the last populated field restores Opened/Not opened according to `OpenedAt`; preserve canceled/finished/expired precedence.

Keep existing GET/HEAD routes and final `POST /customer/{token}`. Replace, not alias, the old draft route with `POST /customer/{token}/draft/{field}` under the existing `draft_save` logging operation. Accept only registry keys. Unknown field returns the normal branded 404. For a known field, parse only its same-named POST parameter: `name`, `email`, `phone`, or `municipality`; require exactly one value so missing/duplicate parameters cannot silently clear data. Empty string explicitly clears. Ignore unrelated form parameters. Email confirmation has no draft endpoint.

In `internal/customer/customer.go`, expand `pageData` to embed `students.Details`, retain `Path/Error/Submitted/Welcome`, and add `EmailConfirm string`, `EmailMismatch bool`, `FieldErrors map[string]string`, and `DraftField string`. Keep raw attempted strings in page data and normalized parsed values separately until a successful store call. Use the registry parser for each known draft field with `required=false`; final submission applies all four parsers with `required=true`, collecting errors for every invalid field in one response.

Final wire fields are exactly `name`, `email`, `email_confirm`, `phone`, `municipality`. Parse confirmation through `ParseEmail(..., true)`; compare only when both email parses succeed. Comparison preserves local-part case and ignores domain case/surrounding whitespace. Mismatch errors belong to `email_confirm`, with `EmailMismatch` distinguishing mismatch from syntax failure in response markup. Never autofill confirmation from saved Email, persist it in either model, or treat draft Email as confirmed.

Replace the current method-value selection (`save := store.Submit`) with explicit draft/final calls because their new signatures intentionally differ. Keep the 4 KiB encoded request-body ceiling and five-second context timeout. No new per-name size limit; existing request limit remains authoritative.

Preserve current response precedence and privacy:
- For known draft fields and final submission, malformed/oversized/invalid input still checks link state before showing input errors: unusable link 404; completed final 200 confirmation; completed draft 409 with HTMX refresh; only usable unfinished links receive 400/413/422.
- Successful draft returns only its field status fragment; final success swaps `#customer-content` or returns a full page without JavaScript.
- Validation failures preserve all attempted fields and selection, including confirmation. Save failures preserve the same attempted data. Parse failures retain persisted details because the request could not be reliably read.
- Lookup/save failures remain honest 500 responses, never success. Keep `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, and current HTMX refresh behavior.
- Completion resets all four details, confirmation, mismatch state, and field errors before rendering, including replay of malformed or invalid attempts. No private values appear in confirmation HTML.

### 3. Replace the question with the mobile student-information form

In `internal/customer/templates/index.html`, use `lang="es-PR"` throughout the customer document. Retain the existing welcome content/logo and ordinary Próximo link. Form heading/title: `Información del estudiante` / `Información del estudiante | Masterminds`. Render one column in this order:

1. `Nombre completo` — `name`, text, `autocomplete="name"`; hint `Escribe tu nombre y apellidos. Solo letras y espacios.`
2. `Correo electrónico` — `email`, email, `autocomplete="email"`, `maxlength="254"`, spellcheck/autocapitalization disabled.
3. `Confirmar correo electrónico` — `email_confirm`, email, `autocomplete="off"`, same length/text-entry settings. Permit paste. Hint `Vuelve a escribir tu correo electrónico.`
4. `Número de teléfono` — `phone`, tel, `inputmode="tel"`, `autocomplete="tel"`; hint `Puerto Rico o Estados Unidos. Incluye el código de área.`
5. `Municipio` — native required select; first option `value=""` with text `Selecciona tu municipio`; then shared 79 choices.

All five controls are required for final submission. Keep regular action/method fallback, current section swap, and final button disabling. Final button: `Enviar información`; success heading/message/title: `¡Gracias!`, `Recibimos la información del estudiante.`, `Información recibida | Masterminds`. Do not label this as completed payment or secured enrollment.

Give each persisted-field control its own `hx-post="{{.Path}}/draft/<key>"`, `hx-params="<key>"`, `hx-trigger="change"`, `hx-sync="this:queue last"`, and `hx-target="#save-status-<key>"` with innerHTML swap. Preserve `hx-disinherit="*"` on the final form so autosaving controls do not inherit its disabling/swap. No `hx-validate="true"` on draft controls: required-but-empty drafts must clear successfully. Per-input queueing is sufficient because each draft updates one column; final SQL guards handle submit/draft races.

Each field wrapper contains a stable label/control, hint, and status/error target. Errors have `role="alert"`, controls link to hints/status through `aria-describedby`, and server-rendered invalid controls use `aria-invalid="true"`. Saved status is `Guardado.`; request failure starts `No se guardó.`. Final form also has an accessible general error region. Do not redraw controls during autosave.

Use these customer field-error strings rather than leaking English parser/library errors:
- Name: `Escribe tu nombre y apellidos usando solo letras y espacios.`
- Email or malformed confirmation: `Escribe un correo electrónico válido.`
- Confirmation mismatch: `Los correos electrónicos no coinciden.`
- Phone: `Escribe un número válido de Puerto Rico o Estados Unidos, con código de área.`
- Municipality: `Selecciona un municipio o “Fuera de Puerto Rico”.`

Global errors: unreadable request `No pudimos leer tu información. Intenta de nuevo.`; failed save `No pudimos guardar tu información. Intenta de nuevo.`; failed lookup `No pudimos abrir este enlace. Intenta de nuevo.`; network failure `No se guardó. Verifica tu conexión e intenta de nuevo.` Unknown municipality attempts render the empty invalid prompt, never silently select a valid municipality or inject a forged option; other attempted fields remain intact.

Create `static/customer.js`; no reusable customer validation script exists. Keep its scope to this form:
- Delegated email/confirmation input handling clears old custom validity before checking native validity, then uses `setCustomValidity` for mismatch only when both values are nonempty and native-valid. Compare using the same trim/domain-lowercase normalization; native required/type=email and authoritative Go parsing handle syntax. Recompute on initialization, input in either email control, and final-error replacement.
- Preserve server-rendered syntax errors on initialization/swaps. Response markup distinguishes confirmation mismatch from syntax failure: clear a stale mismatch message and `aria-invalid` when corrected; clear a stale confirmation syntax error when confirmation is edited. Always remove custom validity when no mismatch remains. `autocomplete="off"` is only a browser hint; the server remains authoritative.
- Update a persisted control's `aria-invalid` from its own draft-validation fragment, using a validation marker distinct from save/network failure. Network failures use the corresponding status or form error region without discarding input. Leave handled server error HTML intact; never turn every unsuccessful request into a generic error or insert raw `xhr.responseText`.
- Focus the first invalid control after a final 422 swap. Query current DOM each time; ignore responses from detached draft controls. Final-response handling must target the newly swapped content, not the detached submitted form, and safely no-op after success.

Do not install a new JS framework, HTMX extension, input mask, or custom select.

Load this script only on form pages; the welcome remains script-free. Add `reportValidityOfForms: true` and an ordered `{"code":"422","swap":true,"error":false}` rule before the existing general 4xx/5xx rule in HTMX configuration. Expected validation responses must swap and retain their field messages, not trigger generic failure overwrites. Preserve the remaining response rules. Standard form submission still invokes identical Go validation when JavaScript is disabled.

Extend existing customer CSS rather than restyle management: preserve JetBrains Mono, brand ink `#25484e`, canvas `#f4f8f8`, input white `#ffffff`, muted `#526b70`, error `#b91c1c`. Keep centered logo/heading and a horizontally centered 28rem-max form, left-aligned labels, input/select text at least 16px, controls at least 44px high, and existing 56px primary action. Apply input styling to native select too. Use `.customer:has(#customer-content form)` to apply form-only top alignment, compact 4.5rem logo, and normal scrolling with safe-area padding; the logo is outside the swapped section, so a render-time body class alone would become stale. Removing the form on HTMX success must automatically restore the existing centered success layout and logo sizing. Update the success title in both full-page and `customer-response` rendering. Do not squeeze five fields into one screen or impose fixed height/overflow clipping.

### 4. Present and edit all details without widening registry exposure

The Students registry order automatically gives Name, Email, Phone, Municipality, then the existing Submitted column in `internal/management/templates/students.html`. Keep Name first so row/detail activation remains attached to it. Detail views and OOB row responses must contain the same canonical values.

Add one branch to `student-field`: fields with `Options` render a native `<select name="value">`; other fields keep the existing input. Preserve stable ID/label, required, validation/error accessibility, modal hidden parameter, save/cancel routes, selected persisted option, and attempted-value error behavior. Use an empty `Select a municipality` prompt; an invalid submitted value selects this prompt and displays the error rather than choosing a municipality. Existing `static/students.js` already focuses selects and excludes them from row activation; existing management CSS already styles selects. Keep those mechanisms.

Staff may edit email directly without a confirmation field; confirmation protects customer entry, not a second canonical column. Shared parsers enforce the same name/email/phone/municipality rules in staff edits. Every edit changes only its canonical field, not source-link snapshot or other students. Retain ordinary POST redirects and HTMX field/OOB responses.

In `internal/management/templates/index.html` shared `link` renderer, show `Entry.Phone` as plain text below the name when nonempty. Keep existing “No name yet”, status, dates, audit, one-time URL/copy, cancel controls, and main/Trash filtering. Do not add email/municipality to `Entry`, hidden HTML, data attributes, or registry responses. Do not add a `tel:` anchor: current copy JS selects the row's first anchor as the customer URL.

Use existing committed-change notifications for live updates; no polling/new SSE work. Verify refreshed rows preserve one-time URLs and Copy link while updating the displayed name/phone.

### 5. Migrate existing callers and strengthen the relevant regressions

LSP references confirm customer handler is the only production caller of the link saves; integration-tagged references need text search as well. Migrate these existing files completely:

- `internal/customer/customer.go`, `internal/customer/customer_test.go`
- `internal/links/links_test.go`
- `internal/students/students_test.go`
- `internal/management/management_test.go`, `internal/management/students_test.go`
- `internal/logging/http_integration_test.go`

`internal/links/links_test.go` contains method-value slices and higher-order helpers typed to the old common save signature, not just direct calls. Replace those test operations with explicit draft/final closures rather than production compatibility adapters. Update helper-generated final submissions to supply complete valid Details. Update any keyed Link/Student literals to initialize embedded Details; unchanged zero-value model literals still work. `Field.Parse` consumers are registry initialization and `students.Store.UpdateField`, plus the new customer field handling.

Replace customer/test/logging `/draft` URLs with `/draft/<field>`, including expected registered logging route `POST /customer/{token}/draft/{field}`. Preserve `draft_save` / `final_submit` operation names. Repository searches for `\.SaveDraft\b|\.Submit\b`, `ParseName\(`, `field\.Parse\(`, and `/draft` locate the full affected set; inspect remaining matches rather than leave deprecated paths.

Adjust test data such as `PRIVATE_ROLLED_BACK_NAME`, `Ada & co`, and successful markup/apostrophe names: they no longer pass name validation. Database-failure tests must use valid full names and complete fields so they still reach SQL and exercise the original rollback/logging contract. Retain XSS/escaping coverage through rejected attempted values or deliberately persisted hostile fixtures, not by weakening the new validator. Remove incidental wording assertions instead of re-pinning translated strings; assert status, field association, preserved data, and no mutation.

In `internal/management/management_test.go`, add Phone to `requireRegistry`'s projected comparison and check nonempty phone rendering in `requireRows`; separate fixture-backed checks assert email and municipality never enter registry HTML. In `internal/management/students_test.go`, replace the `"changed"` value in the database-suppressed update scenario with a valid two-word name so the test still reaches SQL and proves zero-row saves return 404, not a premature validation 422.

## Additional verified findings
- `internal/students/fields.go` exposes the concrete `Field` registry and `ParseName(raw string, required bool) (string, error)`. Current name parser permits punctuation, digits, and markup; the new letters-only contract is a deliberate behavior change.
- `internal/students/students.go` has a generic `UpdateField(ctx context.Context, id uint64, key, raw string) (*Student, error)`; preserve its signature and allowlist.
- `internal/links/links.go` derives In progress solely from Name today; change to any nonempty persisted field. `Entry` is a personal-data projection: add only Phone, not Email/Municipality.
- Existing `static/management.js` treats anchors inside registry rows as customer URLs. Render phone as text, not a `tel:` anchor, to preserve copying without widening unrelated JS.
- Current management live-refresh code and template contracts agree. Earlier test failures are historical evidence, not proof of a current defect. Browser-check refresh; do not expand this task into an unverified live-refresh rewrite.
- Official municipality list: https://pr.gov/gobierno-2/directorio-municipios
- Phone package primary documentation: https://github.com/nyaruka/phonenumbers — import `github.com/nyaruka/phonenumbers/v2`; pin v2.0.15, verified through https://proxy.golang.org/github.com/nyaruka/phonenumbers/v2/@latest on 2026-10-21. Context7 returned unrelated Python results; do not use them for this Go API.
- HTMX 2.0.11 source confirms input-originated drafts do not validate the enclosing form by default, and `hx-params` filters their collected parameters: https://cdn.jsdelivr.net/npm/htmx.org@2.0.11/dist/htmx.js
- Relevant native HTMX features: https://htmx.org/attributes/hx-params/ and https://htmx.org/attributes/hx-sync/ . Keep the pinned v2 behavior; do not use v4 defaults.

## Constraints
- Working tree and system remain read-only until approval through the plan tool.
- Existing data is mock; repository policy prefers straightforward schema changes/reset over backfills. Never reset a remote/shared database without separate authorization.
- Preserve first-open tracking, unfinished-only cancellation, atomic finalization/student insertion, seven-day completed access, replay privacy, and no-JavaScript submission.
- Keep one existing Air dev server on port 3000. No extra server instances.
- A server-side dependency change requires a verified non-production Railway preview before merge or push to master; no push/deployment is authorized by this planning request.

## Verification

No tests or runtime changes were performed during planning. The following checks belong to the approved implementation; report observed results, not inferred passes.

### Regression contracts

Extend the existing test files rather than add a parallel framework or mock-heavy suite. Keep tests for consumer-visible validation, isolation, privacy, and lifecycle invariants:

1. **Shared parsers:** `María José Pérez Rivera` and decomposed accents survive normalization; one-word draft is accepted but one-word final is rejected; digits, hyphens, apostrophes, markup, internal controls, invalid UTF-8, and detached combining marks are rejected. Email accepts plus tags and domain-case normalization, rejects header/display-name syntax, malformed/dotless domains, and overlength addresses. Use representative PR 787/939 and US phone fixtures from the pinned metadata; reject short, Canadian +1, foreign, alphabetic, and extension inputs; verify E.164 storage. Blank required fields and forged municipality values fail; valid accented municipality and outside values succeed.
2. **Partial drafts:** saving or clearing one field leaves all others unchanged, including concurrent edits to two different fields. Blank clears only the requested field; missing/duplicate parameter or unknown key cannot write. Invalid input leaves the last saved value intact. Any populated field marks In progress; clearing all fields restores the open state. Drafts never create Students.
3. **Email confirmation and final validation:** mismatch and malformed confirmation return 422 without enrollment; all attempted values remain in the response. Matching trimmed/domain-normalized values succeed; differing local-part case remains a mismatch. Reload restores saved email but leaves confirmation empty. No confirmation column or persistence exists.
4. **Atomic completion:** one valid submission copies all four normalized values into matching Link snapshot and Student. Concurrent final submissions produce exactly one student containing all four fields from one winning submission, never a mixed record. Failed student INSERT rolls back all link fields, submission time, and expiry. Late drafts and replay cannot change completed values. Retain cancellation races and foreign-key protection.
5. **Access and privacy:** ordinary and HTMX malformed/oversized/invalid requests retain existing unavailable/completed precedence. HEAD is read-only. Completion responses and replays contain none of the submitted four values or confirmation. Extend existing logging assertions to email, phone, municipality, and confirmation as well as name; operation/route metadata stays useful without logging personal data.
6. **Canonical editing:** each staff edit validates and changes only its selected Student field, not another field, another student, or source Link. Invalid select values show an invalid empty prompt without losing other data. Native-select cancel, ordinary redirect, HTMX field response, and modal OOB row reflect the same canonical values. Keep failure/zero-row save regressions reaching SQL with valid fixtures.
7. **Registry projection:** new and refreshed main/Trash rows expose name and phone only as personal information. Email/municipality remain absent even when present in the source record. Snapshot phone stays unchanged after canonical Student edits. Retain status, audit, cancellation, one-time URL, and copy contracts. Existing live updates must refresh displayed phone without losing an already revealed URL.

### Commands and local prerequisites

Work from `/home/gabrielvelez/projects/PaymentWebsite`. Reuse the existing Air server and Compose database. Check port 3000 before starting anything; only if nothing listens, start the single `go tool air` instance. Application `.env` must already provide `DATABASE_URL` and `SESSION_COOKIE_SECURE=false`.

If the database is stopped, use `docker compose up --detach --wait --wait-timeout 90 db`. Use the dedicated local test database, never the application or a remote database. If `paymentwebsite_test` does not exist, create it with `docker compose exec -T db createdb -U paymentwebsite paymentwebsite_test`.

After integrated edits:

```sh
go test ./internal/students
env TEST_DATABASE_URL='postgres://paymentwebsite:paymentwebsite@127.0.0.1:5432/paymentwebsite_test?sslmode=disable' go test -race -tags=integration ./...
```

Do not weaken tests to hide failures. Distinguish newly introduced failures from evidenced unrelated changes; historical management failures are not proof of a current defect.

### Real browser and HTTP smoke

Use fresh local links created through management and existing local staff access. Capture the actual pages at 320×568, 390×844, and 1440×900. Do not send email/SMS or contact the phone fixtures.

1. Welcome remains unchanged: logo, Puerto Rican copy, no removed phrase, Próximo opens the form. Form is one column, readable without horizontal page scrolling, with usable native dropdown, visible labels/focus, touch-sized controls, and natural vertical scroll. Check exactly 79 selectable locations plus the empty prompt, accent rendering, and outside selection.
2. Fill fields in different orders and inspect actual draft requests: each contains exactly its corresponding named parameter, including an explicit empty value when clearing. Reload preserves every valid saved field and leaves confirmation blank. Clear one field and verify others survive.
3. Mismatched emails prevent final submission in the browser. Correct either email and verify stale mismatch text/custom validity clears. Submit a browser-valid but server-invalid name to force a real 422: all inputs survive, field error is not replaced by a generic HTMX failure, and focus reaches the invalid control. Correct the name and submit successfully.
4. Observe success without reload: title, centered layout, normal logo sizing, and Spanish confirmation replace the form; no private data remains in response HTML. Inspect the resulting database rows and Students table/detail to verify all four normalized fields match the submission. Reopen the link to confirm it is locked.
5. Edit each canonical field through the staff UI, including municipality select and phone normalization. Verify table/modal/OOB consistency and source snapshot isolation. Keep a registry tab open during draft and canonical edits: phone updates from link drafts, canonical edits do not rewrite it, and Copy link still copies the customer URL after refresh.
6. Exercise ordinary non-HTMX form POSTs as the no-JavaScript submission path: valid complete payload succeeds; invalid payload returns the complete usable form with all attempted values and field errors. Exercise failed connectivity in the browser and verify honest error feedback with input retained.

