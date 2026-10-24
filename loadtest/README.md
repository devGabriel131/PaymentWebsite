# k6 load and safety scripts

Scripts for the deployed app (`BASE_URL`, default `https://paymentwebsite-production.up.railway.app`). Tokens come from a JSON
array of 43-char link tokens at `TOKENS_FILE` (default `/tmp/k6/tokens.json`); each script uses `OFFSET`..`OFFSET+COUNT`.
One link is one customer and can be submitted once, so use disjoint slices per script and never reuse a token across runs.
The summary JSON goes to `OUT` (default `/tmp/k6/out/<script>.json`); run `mkdir -p /tmp/k6/out` first.

- `journey.js` load test: welcome page to plan selection, `PROFILE=100|1000`. Never POSTs `/payments`. ~850 / ~16000 tokens.
- `safety.js` single-pass contract checks (tokens, drafts, submit, gating, cross-origin, staff, webhook, static, login limiter). Needs 6 tokens.
  The login limiter group runs last and blocks staff sign-in for ~25 s afterwards.
- `checkout.js` starts real Stripe Checkout Sessions and races duplicate `POST /payments`: `PLAN=full|three|six`, `COUNT<=20` (default 1),
  `RACE` (default 10), `CONCURRENT_TOKENS` (default 1). Prints `CHECKOUT <token> <plan> <url-or-NONE> <distinct-url-count>` per token.
- `lib.js` shared helpers and the journey steps.

```sh
D='docker run --rm --network host --ulimit nofile=1048576:1048576 -v /home/gabrielvelez/projects/PaymentWebsite/loadtest:/loadtest -v /tmp/k6:/tmp/k6'
$D -e PROFILE=100 -e OFFSET=0 -e COUNT=1500 grafana/k6:latest run /loadtest/journey.js
$D -e OFFSET=2000 -e COUNT=40 grafana/k6:latest run /loadtest/safety.js
$D -e PLAN=full -e OFFSET=2100 -e COUNT=5 -e RACE=10 grafana/k6:latest run /loadtest/checkout.js
```
