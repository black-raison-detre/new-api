# OpenRouter support improvements

Recorded: 2026-10-21. Assessment baseline: `6cf0d90b5` on
`v1.0.0-rc.45-custom`. The dedicated adaptor, native Messages, returned-cost
billing, per-model routing, endpoint metadata, provider-option preservation, and
the richer cached model catalog are implemented. Pricing imports, authorized
cross-model fallback, and live routing-affinity validation are pending.

## Goal

Use OpenRouter's supported request formats, model catalog, pricing, and provider
metadata while preserving existing channel configurations and web UI contracts.
Let OpenRouter perform upstream protocol conversion where it supports the incoming
format; retain new-api's authorization, validation, billing, and logging controls.

## Initial assessment findings

- OpenRouter is channel type `20` and uses the shared OpenAI adaptor
  (`relay/relay_adaptor.go`, `relay/channel/openai/adaptor.go`).
- Anthropic Messages and Gemini requests are converted locally to Chat
  Completions. OpenRouter documents native `/api/v1/messages`; native Gemini
  `generateContent` and `streamGenerateContent` support remains unverified.
- Responses requests already have a direct forwarding path, although
  `common/endpoint_type.go` advertises only the OpenAI Chat endpoint.
- Normal Chat request serialization drops unknown top-level fields, including
  OpenRouter's `provider` routing options (`relaykit/dto/openai_request.go`).
- Generic channel model fetching already calls OpenRouter's model catalog but
  retains only model IDs (`controller/channel_upstream_update.go`).
- OpenRouter pricing sync imports input, output, and cache-read prices into
  legacy ratios. It ignores broader pricing metadata and context-dependent
  overrides (`controller/ratio_sync.go`).
- Provider endpoint availability, pricing, and performance metadata have no
  dedicated integration.
- Existing pricing sync supports expressions, previews, source selections, and
  conflict handling (`web/src/features/system-settings/models/upstream-ratio-sync.tsx`).
- Prices are keyed by model name: importing a price can affect other channels
  serving the same name. Upstream metadata must remain channel-specific.
- Legacy OpenRouter Claude settlement can infer cache-write tokens from reported
  cost (`service/text_quota.go`, `service/quota.go`). Native Messages usage needs
  correct normalization before changing this behavior.

## Phase 1: Protocol support and routing options

- [x] Introduce a dedicated OpenRouter adaptor, reusing existing transport and
  protocol response handlers where appropriate.
- [x] Preserve channel type `20`, credentials, base URLs, model mappings, header
  overrides, reasoning behavior, and enterprise response compatibility.
- [x] Forward Chat Completions, Responses, and native Anthropic Messages through
  their matching OpenRouter endpoints. Native Messages is opt-in for both
  existing and new channels; enterprise channels retain the legacy path.
- [x] Keep Gemini conversion until native upstream support is verified.
- [x] Add opt-in OpenRouter returned-cost billing for wallet- and subscription-funded text
  requests before implementing configurable provider routing. Apply the existing
  effective group multiplier and retain existing currency display conversion.
- [x] Add per-model Standard/Nitro/Floor routing, exact provider locks, provider
  discovery, and user/conversation affinity in the existing channel editor.
- [x] Preserve documented OpenRouter provider routing fields without requiring blanket
  body passthrough. Define precedence between client options and administrator
  policy; preserve explicit zero and false values.
- [x] Reject cross-model fallback and opaque model selectors before billing and
  again after outbound overrides, including unconfigured and passthrough paths.
- [ ] Enable cross-model fallback only after enforcing authorization and valid
  pricing for every candidate and settling against the actual selected model or
  authoritative returned cost. Provider failover within one model is supported.
- [x] Retain request bounds, parameter overrides, reservations, settlement,
  refunds, and audit behavior across the implemented single-model paths.
- [x] Correct advertised endpoint capabilities to match verified runtime support.
- [x] Check OpenRouter streaming usage support and update the stream-support
  configuration without changing unrelated adaptors.

### Dedicated adaptor foundation

- `relay/channel/openrouter/adaptor.go` now owns OpenRouter adaptor dispatch and
  channel identity, delegating existing relay behavior to the shared OpenAI
  implementation. `relay/relay_adaptor.go` selects it for OpenRouter.
- The enterprise response DTO lives in `relay/channel/openrouter/dto/`, with the
  original public type name preserved as an alias to avoid an import cycle.
- The initial extraction preserved existing protocol routing without settings,
  frontend, or pricing changes. Native Messages was added subsequently below.
- Native URL/header overrides have matching transport dispatch: the outer
  adaptor invokes the shared transport, avoiding embedded-receiver dispatch.
- Focused compatibility tests are in
  `relay/channel/openrouter/adaptor_test.go`. Independent working-tree review
  found no remaining issues.
- Validation passed (working tree, exit 0):

  ```sh
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test ./relay/channel/openrouter ./relay/channel/openai ./relay
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test ./service -run 'OpenRouter|Claude'
  ```

  Coverage includes existing endpoint routing, header overrides, reasoning/usage
  requests, enterprise success/error responses, Chat streaming, Responses, and
  existing OpenRouter/Claude service regressions. `git diff --check` passed.
  Full `go test ./...` and browser validation were not run. Authenticated adaptor
  validation is recorded below. No frontend or database behavior was changed by
  this extraction.

### Live adaptor verification (2026-10-21)

- Passed six authenticated checks through the factory-selected
  `openrouter.Adaptor` using `openai/gpt-4.1-nano`: Chat JSON, Chat SSE, Responses
  JSON, Responses SSE, Claude-to-Chat JSON conversion, and Gemini-to-Chat JSON
  conversion. All returned HTTP 200 and the expected `OK` text.
- Each request applied the real model-mapping helper to a test alias and returned
  normalized usage of 14 input tokens and 2 output tokens. Both streams reached
  their completion marker. Chat/conversion requests used
  `/api/v1/chat/completions`; Responses used `/api/v1/responses`.
- Each response reported USD 0.0000022 in usage cost; the six responses total
  USD 0.0000132. Immediate key-usage snapshots remained zero, so this is reported
  response cost, not independent proof of account-level settlement.
- The temporary local harness exercised conversion, transport, and response
  handlers without starting the gateway or its database. Gateway authentication,
  database-backed quota reservation/settlement, the web UI, a live enterprise
  endpoint, and native Messages remain outside this live validation.
- Local command (exit 0):

  ```sh
  GOCACHE=/private/tmp/new-api-openrouter-gocache go build -o /private/tmp/new-api-openrouter-live /private/tmp/new-api-openrouter-live.go
  /private/tmp/new-api-openrouter-live
  ```

  Sanitized results: `/private/tmp/new-api-openrouter-live-results.json`.
  Log: `/private/tmp/new-api-openrouter-live.log`.
  The credential was read from its restricted local file and was not written to
  repository files or test output.

### Native Anthropic Messages (2026-10-21)

- Implemented `settings.openrouter_native_messages` (default false), exposed as
  **Native Anthropic Messages** in the OpenRouter channel editor. All seven
  interface locales include the new copy. Existing settings, credentials, base
  URLs, mappings and administrator overrides retain their current behavior.
- Enabled channels forward Messages to `/v1/messages`, retaining native system,
  tools, thinking and cache-control blocks through the existing Claude request
  preparation and JSON/SSE response handlers. Bearer authentication and OpenRouter
  attribution headers are reused; Anthropic version/beta headers are forwarded.
  Explicit native selection takes precedence over the global Responses upgrade
  policy. Chat, Responses, Gemini conversion and enterprise paths stay unchanged.
- Enterprise accounts disable the native switch and ignore the flag at runtime;
  their existing response-envelope handling remains in effect.
- A live Sonnet 4.6 probe confirmed exclusive ordinary input: 10 input tokens
  plus 1,691 cache-write tokens, then 10 input plus 1,691 cache-read tokens.
  Native accounting skips legacy OpenRouter cache subtraction/cost inference.
  Regressions cover read/write, mixed TTL, fully cached input, and expression
  token normalization; existing legacy billing tests still pass.
- Ten live factory/adaptor cases passed: the six baseline cases above, native
  Messages JSON/SSE with GPT-4.1 nano, and native Claude Sonnet 4.6 JSON cache
  creation/SSE cache reads. Native conversion chains remain `[claude]`, with
  alias mapping applied. The Claude adaptor cases reported 14 ordinary input
  tokens, 4 output tokens and 1,691 cache tokens separately.
- Local validation passed:

  ```sh
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./relay/channel/openrouter ./relay/channel/openai ./relay/channel/claude ./relay ./service -run 'OpenRouter|NativeMessages|Adaptor|Claude'
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./relay/channel/openrouter -count=1
  # Inside relaykit/:
  GOWORK=off GOCACHE=/private/tmp/new-api-openrouter-gocache go build -p 2 ./...
  # Inside web/:
  bun run typecheck
  bun run test src/features/channels/lib/__tests__/openrouter-channel.test.ts src/features/channels/lib/__tests__/aws-channel.test.ts src/features/channels/lib/__tests__/new-api-channel.test.ts
  bunx --no-install oxlint -c .oxlintrc.json src/features/channels/lib/channel-form.ts src/features/channels/lib/__tests__/openrouter-channel.test.ts src/features/channels/components/drawers/channel-mutate-drawer.tsx src/features/channels/types.ts
  bun run build
  bun run i18n:sync
  ```

- Real settings persistence matrix passed SQLite 3.50.4, MySQL 8.0.46 and
  PostgreSQL 15.19 using `go test -p 2 ./model -run
  '^TestInferencePresetSettingsAndDatabaseRoundTrip$' -v` with isolated
  `TEST_MYSQL_DSN`/`TEST_POSTGRES_DSN`. The existing JSON text column is reused;
  no schema or migration change was introduced. Temporary containers were
  removed and the previously stopped Colima VM restored to stopped state.
- Full `ClaudeHelper` routing-policy integration covers native/legacy dispatch,
  model mapping, injected system prompt, explicit-zero parameter overrides and
  upstream 429 propagation. Frontend tests passed 17 cases across three files.
- Rendered production frontend checks passed with mocked management APIs at
  desktop 1440x1050 and narrow 390x844 sizes: default-off, enabled, and disabled
  under Enterprise Account. Screenshots were inspected. These verify rendered
  interaction, not authenticated management API saves. Harness/log:
  `/private/tmp/new-api-openrouter-ui.mjs`,
  `/private/tmp/new-api-openrouter-native-ui.log`; screenshots:
  `/private/tmp/new-api-openrouter-native-desktop.png` and
  `/private/tmp/new-api-openrouter-native-narrow-enterprise.png`.
- Independent working-tree source review approved the native implementation;
  that milestone was committed as `07f604dcb`. At that milestone, full
  `go test ./...`, deployed gateway
  authentication, persistent quota reservation/settlement/refunds and a live
  enterprise endpoint are outside this verification.
- Reproduction artifacts (outside Git, no credential output):
  `/private/tmp/new-api-openrouter-native-live.go`,
  `/private/tmp/new-api-openrouter-native-live-results.json`,
  `/private/tmp/new-api-openrouter-native-live.log`,
  `/private/tmp/new-api-openrouter-cache-probe.log`,
  `/private/tmp/new-api-openrouter-native-db.log`,
  `/private/tmp/new-api-openrouter-native-go.log`,
  `/private/tmp/new-api-openrouter-native-integration.log`,
  `/private/tmp/new-api-openrouter-native-web.log`.

### Returned-cost wallet billing (2026-10-21)

- Channel-level `openrouter_cost_billing` defaults to false. Standard OpenRouter
  channels can charge the final reported USD cost using the existing effective
  group multiplier. Enterprise channels keep local pricing.
- The first version is wallet-only. Preserve the user's funding preference;
  reject subscription-funded requests before forwarding instead of silently
  switching them to a wallet charge.
- Do not reserve wallet credit in advance. Require positive wallet and limited
  API-token balances at admission; actual settlement can leave debt. Currency
  conversion remains a presentation concern, and no new markup control is added.
- Explicit zero cost is valid only at a complete accounting boundary. Missing,
  invalid, or interrupted accounting remains pending for generation-cost lookup;
  never silently substitute a free charge or local model price.
- Persist generation identity and reconcile pending charges durably. Ledger and
  accounting updates must prevent duplicate debits across retries or restarts.
  Preserve administrator-only upstream cost details and public pending status.
- Provider-direct BYOK inference fees are outside the returned OpenRouter charge.
  Catalog prices remain useful metadata, not the settlement source in this mode.
- Implemented the setting in the existing channel editor and pending/settled
  billing details in the existing usage logs and CSV export. Frontend copy is
  present in all seven locales; new AI-client errors use the three backend locales.
- Primary-database settlement atomically updates the ledger, wallet, token,
  request counters and channel usage. Header generation IDs are saved before
  streaming; an active-request heartbeat delays recovery until the request ends
  or its lease expires. Cross-mode channel retries are rejected, and retries stop
  after an upstream generation ID has been acquired.
- Log recovery and foreground completion share one idempotent projection. A
  separate log-database outage does not block charging in the primary database.
  SQLite takes write intent before projecting to a separate log database;
  ClickHouse updates wait for mutation completion. Dashboard rows carry a nullable
  unique request discriminator so ordinary hourly cache flushes cannot alter them.
- Focused backend suites and the returned-cost regressions passed on real
  SQLite 3.50.4, MySQL 8.0.46 and PostgreSQL 15.19, with no matrix skips. Coverage
  includes no reservation, frozen group ratio, negative credit, limited-token and
  subscription admission, explicit zero, missing/incomplete cost, concurrent
  settlement, restart recovery, missing logs, log outages, deferred SQLite
  transactions and mixed ordinary/actual-cost dashboard updates.
- Fresh and released-schema upgrade checks passed twice each on all three primary
  databases (12 startup/migration runs), using the hosted `v1.0.0-rc.45` release
  as the upgrade baseline. Existing balances, channel settings, token uniqueness,
  dashboard aggregates and indexes were retained. The expected additions are the
  ledger and nullable `quota_data.billing_request_id` column/unique index.
- Real ClickHouse 25.8.33.6 log verification passed pending-to-settled mutation,
  immediate final-state readback, one consume log and one wallet debit.
- Frontend validation passed 165 tests across six existing files, typecheck,
  targeted lint, i18n sync and production build. Inspected production-build
  screenshots at 1440x1050 and 390x844 cover channel selection/keyboard use,
  enterprise disabling and administrator/user pending/settled log views.
  Management APIs were mocked for these browser checks.
- Six live factory/adaptor calls (Chat, Responses and native Messages, JSON/SSE)
  all returned HTTP 200 with final cost and generation headers. Initial generation
  lookups sometimes returned 404; later read-only lookups matched every inline
  cost. An additional authenticated gateway call exercised model mapping without
  local pricing, group multiplier 2, a 2-quota debit from a 1-quota wallet to -1,
  exactly one consume log, and rejection of the next request before forwarding.
  Seven responses reported USD 0.0000154 total, below the approved USD 0.01 budget.
- Independent UI and backend source review approved the working-tree feature
  after fixing the reported recovery races. The commit-time audit additionally
  covered the token-deletion and public-log regressions below.
- Precommit review found that soft-deleting a token could block an outstanding
  charge. Settlement now uses the original owner-bound token row for accounting
  even after deletion, while ordinary token access remains revoked. Foreground
  and recovery regressions prove one debit and preserve the deletion marker.
- Actual-cost log projection strips public `group_ratio` and `user_group_ratio`
  while retaining the effective ratio under administrator metadata. Real user
  and token-log API regressions verify privacy and retain ordinary local-pricing
  metadata. The updated model/service/relay suites passed on all three primary
  databases without skips; the complete root suite passed again with only the
  documented release-baseline WebSocket fixture excluded. No schema changed in
  these precommit fixes.
- Strict full-root `go test ./...` was run. Its only failure after the required
  sandbox escalation was the pre-existing
  `TestAdaptorMasksDialedHostsInDialFailure/channel_proxy_host` WebSocket fixture;
  the same failure reproduced on the untouched release baseline. The complete
  root suite excluding only that exact fixture passed. Final targeted checks
  passed for backend locale rendering, numeric bounds and invalid-context errors
  after the last localization fix. This is not an unmodified full-suite pass.
- Removed the three isolated database containers and restored the previously
  stopped Colima VM to stopped state. No existing containers were removed.
- Reproduction commands (from the root unless noted; database commands used
  isolated `TEST_MYSQL_DSN` and `TEST_POSTGRES_DSN`):

  ```sh
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./model ./service ./relay ./relay/channel/openrouter ./relay/channel/openai ./relay/channel/claude -count=1
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./... -count=1
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./... -count=1 -skip '^TestAdaptorMasksDialedHostsInDialFailure$/^channel_proxy_host$'
  # Inside relaykit/:
  GOWORK=off GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./dto -count=1
  GOWORK=off GOCACHE=/private/tmp/new-api-openrouter-gocache go build -p 2 ./...
  # Inside web/:
  bun run test src/features/channels/lib/__tests__/openrouter-channel.test.ts src/features/channels/components/__tests__/channel-configuration.test.tsx src/features/usage-logs/components/__tests__/cost-display.test.tsx src/features/usage-logs/components/__tests__/usage-facts.test.tsx src/features/usage-logs/components/__tests__/detail-preview.test.tsx src/features/usage-logs/lib/__tests__/export.test.ts
  bun run typecheck
  bun run build
  bun run i18n:sync
  ```

- Local verification artifacts: `/private/tmp/new-api-openrouter-cost-service-matrix.log`,
  `/private/tmp/new-api-openrouter-cost-backend-tests.log`,
  `/private/tmp/new-api-openrouter-cost-final-migration-{engine}-{fresh,upgrade}-{1,2}.log`,
  `/private/tmp/new-api-openrouter-cost-clickhouse.log`,
  `/private/tmp/new-api-openrouter-cost-live-results.json`,
  `/private/tmp/new-api-openrouter-cost-lookup-results.json`,
  `/private/tmp/new-api-openrouter-cost-gateway.log`,
  `/private/tmp/new-api-openrouter-cost-logs-browser.log`, and
  `/private/tmp/new-api-openrouter-cost-full-go-unsandboxed.log`,
  `/private/tmp/new-api-openrouter-cost-full-go-excluding-baseline.log`, and
  `/private/tmp/new-api-openrouter-cost-final-guards.log`,
  `/private/tmp/new-api-openrouter-cost-precommit-regression-before.log`,
  `/private/tmp/new-api-openrouter-cost-precommit-fixed-tests.log`, and
  `/private/tmp/new-api-openrouter-cost-precommit-full-go.log`.

### Subscription-funded returned-cost billing (2026-10-21)

- Returned-cost billing now supports subscription allowances as well as wallets.
  It follows `subscription_only`, `subscription_first`, `wallet_only`, and
  `wallet_first`; subscription-first wallet fallback retains the existing
  `allow_wallet_overflow` policy. Unlimited plans and automatic resets are
  handled before admission. A subscription-funded request needs no wallet credit.
- Neither funding source reserves quota upfront. The final reported cost uses
  the frozen effective group multiplier and can exceed the remaining allowance.
  An exhausted subscription cannot fund another request until reset; a permitted
  fallback may instead select the wallet for a subsequent request. An incurred
  subscription charge is never silently moved to a wallet or another subscription.
- The ledger freezes the funding source, subscription, plan, and allowance cycle
  before forwarding. Settlement and recovery atomically apply one charge and
  project the subscription details into logs. Expiry, cancellation, or deletion
  does not redirect the charge. A late result from an older cycle stays in the
  ledger/log without consuming the refreshed allowance.
- Durable cycle versions distinguish even same-second administrative resets.
  Ordinary-request refunds can reduce postpaid overage, and a reservation from an
  old cycle cannot refund quota into a new one. Admission, settlement, cancellation,
  deletion, and expiration use consistent user-before-subscription row locking.
- Six returned-cost database matrices passed without skips on SQLite 3.50.4,
  MySQL 8.0.46, and PostgreSQL 15.19, including funding preferences, concurrent
  admission/settlement, overage, reset/expiry/deletion, recovery, and mixed refunds.
- Full startup checks passed twice on fresh and released `v1.0.0-rc.45` databases
  for all three engines (12 current starts). Existing balances, subscription
  reservations, and all released indexes were retained, with no index drift on
  the second start. An additional upgrade from wallet-billing commit `6ec432838`
  passed twice per engine and preserved pending wallet ledgers with zero-valued
  subscription snapshots (six more current starts).
- Real ClickHouse 25.8.33.6 logging passed pending-to-settled projection with one
  subscription charge, one consume log, and an unchanged wallet. An authenticated
  production-router check against a TLS mock upstream charged USD 0.002 times
  group multiplier 2 as 2,000 subscription quota with an empty wallet, then rejected
  the next request before forwarding. This was not a live OpenRouter call.
- Independent backend review approved the final source after the concurrency
  fixes. The strict root suite encountered the already documented
  `TestAdaptorMasksDialedHostsInDialFailure/channel_proxy_host` baseline failure;
  the final root suite passed with only that case excluded. Other unrelated
  environment-dependent test skips remain; the six OpenRouter matrices had none.
- Frontend validation passed 148 tests across four existing channel/usage-log
  files, typecheck, targeted lint, i18n sync, and the production build. Inspected
  desktop and phone screenshots and exercised keyboard selection and enterprise
  disabling with mocked management APIs. The channel explanation is translated
  in all seven locales and reuses the existing setting components.
- Reproduction commands (root unless noted; the wrappers load isolated test DSNs):

  ```sh
  python3 /private/tmp/new-api-subcost-run-full-tests.py
  # Executes with GOCACHE=/private/tmp/new-api-openrouter-gocache:
  go test -v -p 2 ./... -count=1 -timeout=3m -skip '^TestAdaptorMasksDialedHostsInDialFailure$/^channel_proxy_host$'
  python3 /private/tmp/new-api-subcost-run-migrations.py
  GOCACHE=/private/tmp/new-api-openrouter-gocache go build -p 2 -o /private/tmp/new-api-subcost-gateway /private/tmp/new-api-subcost-gateway.go
  /private/tmp/new-api-subcost-gateway
  # Inside web/:
  bun run test src/features/channels/lib/__tests__/openrouter-channel.test.ts src/features/channels/components/__tests__/channel-configuration.test.tsx
  bun run test src/features/usage-logs/components/__tests__/cost-display.test.tsx src/features/usage-logs/components/__tests__/detail-preview.test.tsx
  bun run typecheck
  bunx oxlint -c .oxlintrc.json src/features/channels/components/drawers/channel-mutate-drawer.tsx
  bun run i18n:sync
  bun run build
  ```

- Evidence is under `/private/tmp/new-api-subcost-*`, including
  `full-go-excluding-baseline.log`, the earlier strict `full-go.log`, per-engine
  `migration-*` and `wallet-current*` logs, `clickhouse.log`, `gateway.log`,
  `log-ui-tests.log`, `browser.log`, and desktop/phone screenshots.

### Per-model routing and provider affinity (2026-10-22)

- Implemented `settings.openrouter_model_routing`, keyed by the published model
  name in each OpenRouter channel. Each entry selects Standard, Nitro, Floor,
  or a locked provider endpoint. Existing channels and manually suffixed mappings
  keep their behavior until a model policy is explicitly edited.
- The existing **Configure Models** dialog now edits policies per model, displays
  routing badges, and supports another published alias for the same upstream
  model through the shared mapping editor. Apply updates the channel draft;
  Cancel discards dialog edits; Update Channel persists models, mappings, and
  policies together. All seven frontend locales contain the new copy.
- Locked providers use exact endpoint slugs, including region/tier variants,
  with a hard allowlist and disabled fallback. Automatic modes use native
  `:nitro` / `:floor` suffixes after model mapping. The final outbound boundary
  enforces configured policy after parameter overrides and body passthrough;
  model-changing fallbacks, remote routers, and presets are rejected on these
  configured paths. Original authorization and billing model names are retained.
- Added the additive administrator endpoint
  `POST /api/channel/openrouter/endpoints`, requiring channel-sensitive-write
  permission. It supports saved and unsaved connection settings, bounded
  requests/responses, timeouts, credential-separated caching, last-success data,
  and exact provider IDs. Existing model fetching still returns model-ID arrays.
  The richer catalog and endpoint metadata are recorded in Phase 2 below;
  pricing-import work remains open.
- Conversation keys follow body `session_id`, `x-session-id`, and
  `prompt_cache_key`, with stable opening-message fallback. Upstream identifiers
  are opaque and scoped to authenticated user, model/policy, group, and account.
  A bounded Redis/in-memory cache prefers the same eligible channel and enabled
  credential, including multi-key channels, for ten minutes after success.
  Retries can release the preferred credential; changed access, configuration,
  or disabled credentials invalidate its use.
- OpenRouter owns provider affinity. Its documented ten-minute idle timeout,
  provider failover, and upstream cache expiry still apply. Explicit provider
  locks take precedence. Nitro/Floor affinity precedence is undocumented and
  has not been verified by live OpenRouter calls in this milestone.
- Enterprise channels retain their existing behavior and cannot enable these
  per-model policies in this release. No database schema or migration changed.
- Final working-tree review approved the source after fixing preset bypasses,
  provider constraint intersections, chained mappings, operator permissions,
  credential retry recovery, and the existing Chat/Claude-to-Responses conversion
  path. Completed/incomplete stream regressions protect affinity recording.
- Focused feature tests passed without skips, including real settings round trips
  on SQLite 3.50.4, MySQL 8.0.46, and PostgreSQL 15.19. Independent `relaykit`
  DTO tests and its standalone build passed. No schema migration was introduced.
- Frontend validation passed 156 tests across five existing/new feature test
  files, type checking, targeted lint/format checks, i18n sync, and production
  build. Inspected production screenshots and exercised keyboard selection,
  exact provider IDs, chained aliases, and atomic saves at desktop 1440x1050,
  phone 390x844, and an additional 1280x1000 alias-chain view. Management APIs
  were mocked for browser checks; backend provider discovery used HTTP fixtures.
- Strict root `go test ./...` was run. The first run exposed a fixture-only
  PostgreSQL DSN syntax problem (corrected to URL syntax) and the previously
  documented `TestAdaptorMasksDialedHostsInDialFailure/channel_proxy_host`
  baseline failure. The final complete root suite passed with only that exact
  subcase excluded; it is not an unmodified full-suite pass. Other unrelated,
  environment-dependent tests still skip; the explicit routing/settings matrix
  had no skips.
- One intermediate full-suite run also failed
  `TestResponsesWebSocketReusesConnectionAndSettlesEachRequest`, admitting a third
  request when the fixture expected an exhausted-token rejection and two logs
  (`controller/responses_websocket_test.go:605`). The isolated test passed on
  both this source and clean HEAD; the clean-HEAD controller suite and the final
  current full-root suite passed. This remains an intermittent validation
  finding to investigate, not a reproduced OpenRouter-routing defect. No extra
  test exclusion or source change was used to obtain the final pass.
- Security applicability review used OWASP ASVS 5.0.0 authorization controls
  8.2.1, 8.3.1, and 8.3.2, the
  [Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html),
  and the [Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html).
  Discovery reuses server-enforced channel-sensitive-write permission; affinity
  rechecks current channel eligibility and enabled credentials. Inference
  affinity IDs do not authenticate users or replace browser sessions. This is
  an applicability assessment, not an ASVS compliance claim.
- Reproduction commands (root unless noted; the matrix additionally requires
  isolated `TEST_MYSQL_DSN` and URL-form `TEST_POSTGRES_DSN`):

  ```sh
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -v -p 2 ./... -count=1 -timeout=3m -skip '^TestAdaptorMasksDialedHostsInDialFailure$/^channel_proxy_host$'
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -v -p 2 ./controller ./model ./service ./middleware ./relay ./relay/channel/openrouter ./relay/channel/openai ./relay/common -run 'OpenRouter|FetchOpenRouterEndpoints|InferencePresetSettingsAndDatabaseRoundTrip' -count=1
  # Inside relaykit/:
  GOWORK=off GOMODCACHE=/private/tmp/new-api-routing-modcache GOCACHE=/private/tmp/new-api-routing-gocache go test -p 2 ./dto -count=1
  GOWORK=off GOMODCACHE=/private/tmp/new-api-routing-modcache GOCACHE=/private/tmp/new-api-routing-gocache go build -p 2 ./...
  # Inside web/:
  bun run test src/features/channels/components/__tests__/channel-configuration.test.tsx src/features/channels/components/__tests__/model-mapping-editor.test.tsx src/features/channels/components/__tests__/openrouter-model-routing.test.tsx src/features/channels/components/__tests__/upstream-model-selection.test.tsx src/features/channels/lib/__tests__/openrouter-channel.test.ts
  bun run typecheck
  bun run build
  bun run i18n:sync
  ```

- Sanitized local evidence is under `/private/tmp/new-api-routing-*`, including
  `full-go-final.log`, `final-focused.log`, `web-tests.log`, `web-typecheck.log`,
  `web-build.log`, `browser.log`, and desktop/phone/alias-chain screenshots. No
  live OpenRouter inference, provider-affinity precedence, or cache-hit test was
  performed in this milestone.
- Removed the two isolated database containers and their temporary credentials;
  restored the previously stopped Colima VM to stopped. Existing images and the
  user's build container were retained.

### Phase 1 capability and routing safeguards (2026-10-22)

- OpenRouter endpoint metadata now advertises Chat and Responses. Pricing/model
  metadata also includes Anthropic Messages when at least one enabled channel
  for that model opts into native Messages and is not enterprise. Channel-cache
  updates invalidate the metadata when the setting changes. No schema changed.
- Public OpenRouter channels retain Chat `stream_options` through both channel
  initialization and the shared OpenAI converter. Converted Claude/Gemini
  streams request usage; native Messages and enterprise behavior are preserved.
  OpenRouter's current [usage accounting documentation](https://openrouter.ai/docs/cookbook/administration/usage-accounting)
  says usage is automatic and the legacy inclusion parameters have no effect;
  they are retained for request compatibility, not relied on for accounting.
- Raw `provider` options survive typed request serialization for Chat, legacy
  Completions, Responses, native Messages, and converted Claude/Gemini requests.
  Explicit zero/false provider values remain present. Administrator outbound
  preferences take precedence; configured model/provider policies remain final.
  Client/admin allowlists intersect, denylists combine, and privacy/parameter
  requirements cannot be loosened. Conflicting unsupported restrictions reject
  instead of silently relaxing a price cap or other condition.
- Provider failover stays available within the authorized model. Cross-model
  lists, presets, remote routers, and other opaque selectors are rejected with
  a non-retryable 400 before pricing/reservation and checked again at the final
  outbound boundary. These checks also cover passthrough, legacy Completions,
  non-JSON content-type headers, and arbitrary `Upgrade` headers. Unconfigured
  same-model `:exacto` requests retain their existing behavior. Full cross-model
  fallback support remains explicitly open above; this milestone does not add it.
- Regression tests extend existing suites and verify provider preservation,
  restrictive override precedence, pre-reservation rejection, no unsafe upstream
  forwarding, streaming conversion, native/enterprise capability selection,
  shared-model capability union, and settings invalidation with/without cache.
- Metadata database verification passed without skips on SQLite 3.50.4,
  MySQL 8.0.46, and PostgreSQL 15.19, using isolated databases. The matrix exercises
  real channel storage, cache/database settings reads, capability aggregation,
  and setting removal. This change adds no migration or settlement behavior.
- Independent working-tree review approved the final nine code/test files after
  fixing legacy Completions coverage, the Upgrade-header bypass, and same-model
  Exacto compatibility. This initial review is separate from the staged review
  required before committing.
- Security applicability review consulted OWASP ASVS 5.0.0 authorization controls
  8.2.1, 8.2.2, and 8.3.1, the
  [Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html),
  [Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html),
  and [Authorization Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html).
  Opaque selection is denied at the trusted gateway boundary; existing user/model
  authorization remains authoritative. This is an applicability assessment,
  not an ASVS compliance claim.
- Validation commands (root, external DSNs must point to disposable databases):

  ```sh
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./common ./model ./relay/channel/openrouter ./relay/channel/openai ./relay/common ./relay ./service -run 'OpenRouter|Pricing.*Endpoint|PricingOpenRouter|NativeMessages|ChatCompletionsViaResponses' -count=1 -timeout=3m
  TEST_MYSQL_DSN='<isolated MySQL DSN>' TEST_POSTGRES_DSN='<isolated PostgreSQL URL DSN>' GOCACHE=/private/tmp/new-api-openrouter-gocache go test -v -p 2 ./model -run '^TestPricingOpenRouterEndpointTypesDatabaseMatrix$' -count=1 -timeout=3m
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./... -count=1 -timeout=3m
  GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./... -count=1 -timeout=3m -skip '^TestAdaptorMasksDialedHostsInDialFailure$/^channel_proxy_host$'
  ```

- The initial focused seven-package run passed. Strict root `go test ./...`
  failed only the previously recorded release-baseline
  `TestAdaptorMasksDialedHostsInDialFailure/channel_proxy_host` case. The final
  post-review root suite passed with only that exact subcase explicitly excluded;
  this is not an unmodified suite pass. Environment-gated tests retain their
  existing skips; the dedicated metadata database matrix had no skips.
  No `relaykit`/`tokenkit` source or frontend changed. No live OpenRouter call or
  production acceptance was verified. Local logs are
  `/private/tmp/new-api-phase1-focused.log`,
  `/private/tmp/new-api-phase1-db-matrix.log`,
  `/private/tmp/new-api-phase1-full-go.log`, and
  `/private/tmp/new-api-phase1-full-go-final.log`.
- Removed the temporary MySQL/PostgreSQL containers, their volumes, and temporary
  credentials, then restored the previously stopped Colima VM to stopped. Existing
  images and the user's build container were retained.

## Phase 2: Catalog and provider metadata

- [x] Add a cached OpenRouter catalog service retaining capabilities, modalities,
  context/output limits, supported parameters, and raw pricing metadata.
- [x] Keep existing model-fetch responses as arrays of model IDs; expose richer
  metadata through an additive API.
- [x] Evaluate account-filtered discovery separately from the public catalog.
  Never expose models merely because OpenRouter lists them; preserve local
  channel enablement and user access controls.
- [x] Fetch per-model provider endpoint details on demand, with bounded requests,
  timeouts, caching, timestamps, and last-successful-data fallback.
- [x] Treat unavailable or null provider metrics as unknown. Do not automatically
  disable channels based on upstream catalog or health changes.

### Catalog metadata implementation (2026-10-22)

- Added `POST /api/channel/openrouter/catalog`, guarded by the same administrator
  authentication and channel-sensitive-write permission as endpoint discovery.
  It accepts the existing saved/preview connection fields plus `scope`:
  `public` (default) fetches `/models?output_modalities=all`; `account` fetches
  `/models/user?output_modalities=all` with the selected channel key. Account scope
  applies OpenRouter's provider preferences, privacy settings, and guardrails;
  it is discovery data, not authorization to use any listed model locally.
- Added token-authenticated `GET /v1/models/metadata/openrouter`. It uses the same
  group, token model restrictions, and billing-configuration eligibility helper
  as `/v1/models`, then returns exact model-ID matches from the public catalog.
  Each request applies local eligibility again, including when metadata is cached.
  Models without a matching upstream ID are omitted; aliases are not guessed.
  The route accepts no upstream connection settings or credentials.
- Both catalog APIs return `data.models` entries shaped as `{id, metadata}`.
  `metadata` is the complete upstream model JSON object, preserving capabilities,
  input/output modalities, limits, supported parameters, raw pricing strings,
  unknown fields, and explicit null/zero/false values. `data` also records
  `source: "openrouter"`, `scope`, `fetched_at`, and `stale`. An empty local
  eligibility set returns an empty array without an upstream request (timestamp 0).
  Existing model-fetch ID arrays and the normal `/v1/models` formats are preserved.
- The model cache uses a five-minute TTL, eight retained identities, four
  simultaneous fetches, and a ten-second deadline. Scope, credentials, base URL,
  proxy, and header overrides are hashed into separate cache identities. Responses
  are cloned, and failed refreshes return last-successful data marked stale with
  its original timestamp. This is an in-memory cache, not persistent storage.
- Catalog HTTP fetches are capped at 16 MiB / 8,192 models; endpoint fetches keep
  their 2 MiB / 512-row bounds. Redirects are not followed, request errors omit
  credentials and upstream bodies, and partial paginated catalog responses are
  rejected rather than following upstream-provided links. Requests omit pagination
  parameters to request the documented full list.
- Existing on-demand endpoint results keep `id`, `name`, and `provider_name`,
  and add `metadata` containing the complete endpoint row. Provider-specific
  prices, limits, supported parameters, and nullable performance metrics survive.
  Null or unavailable metrics remain unknown. Discovery changes no enabled
  abilities, channel health, configured prices, reservations, or settlement.
- Authorization review follows OWASP ASVS 5.0.0 V8.2.1–V8.2.3 and V8.3.1–V8.3.2
  for function/data/field permissions and trusted server-side enforcement, with
  the [Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html),
  [Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html),
  and [Authorization](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html)
  guidance. Existing authentication/session mechanisms were not modified; this
  is not a whole-application ASVS compliance claim.
- Focused regressions passed:
  `GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./service ./controller ./router -run 'OpenRouter|ListModels' -count=1 -timeout=3m`.
  Coverage includes raw metadata, credential/scope isolation, cloning, cancellation,
  bounded work, stale timestamps, request/response bounds, redirect rejection,
  unchanged ID-only discovery, local group/token/billing restrictions, and route
  authentication/permission guards. No new database queries, schemas, drivers,
  frontend contracts, or independent-module APIs were changed.
- Live read-only public discovery passed with 468 models and one endpoint for
  `recursive/flint-v1.0:free`:
  `TEST_OPENROUTER_PUBLIC_CATALOG=1 GOCACHE=/private/tmp/new-api-openrouter-gocache go test -v ./controller -run '^TestOpenRouterPublicCatalogLive$' -count=1 -timeout=1m`.
  The first live run caught valid upstream `~` model IDs rejected by the discovery
  validator; the corrected validator and regression now preserve them without
  changing local authorization. Evidence: `/private/tmp/new-api-phase2-live.log`.
  This proves public metadata retrieval, not authenticated account discovery,
  inference, billing, or live routing/affinity. No upstream API key was used.
- Initial working-tree review approved the nine code/test files at fingerprint
  `511d2c5b283d253224555fab43dec2f8c7139936` with no actionable defects. This is
  working-tree approval, not staged precommit approval. `git diff --check` passed.
- The strict root command
  `GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./... -count=1 -timeout=3m`
  ran and failed only the known release-baseline
  `TestAdaptorMasksDialedHostsInDialFailure/channel_proxy_host` in
  `relay/channel/advancedcustom`: localhost WebSocket unexpected EOF instead of
  the expected masked proxy host. Evidence: `/private/tmp/new-api-phase2-full-go.log`.
  The final command below passed (exit 0, about 72 seconds), excluding only that
  subtest; this is not an unmodified full-suite pass:
  `GOCACHE=/private/tmp/new-api-openrouter-gocache go test -p 2 ./... -count=1 -timeout=3m -skip '^TestAdaptorMasksDialedHostsInDialFailure$/^channel_proxy_host$'`.
  Evidence: `/private/tmp/new-api-phase2-full-go-final.log`. Tests exercised the
  working tree matching the initial `511d2c5b283d253224555fab43dec2f8c7139936`
  code fingerprint, before the authenticated probe below, not an isolated staged
  checkout. Environment-gated tests retain their usual skips; the separate live
  public discovery command above ran without skipping.

### Authenticated discovery verification (2026-10-22)

- Used the operator-provided key from a private file outside the repository;
  neither the key nor key-info response bodies are printed or stored in logs.
  Added the opt-in `TestOpenRouterAuthenticatedCatalogLive` to the existing
  controller regression file. It bounds the key-file read, requires private file
  permissions, and uses only read-only metadata/key requests with redirects disabled.
- Credential validation passed independently: `GET /api/v1/key` returned HTTP 200
  for the real key and HTTP 401 for a deliberately invalid constant key. The first
  probe found that `/models/user` also returned a catalog with an invalid key;
  metadata discovery success must not be treated as credential validation or
  local authorization. Invalid-key fallback is a live observation, not an API
  guarantee or a production requirement imposed by the tests.
- Live testing caught a catalog completeness gap: public `/models` defaults to
  text-output models too. Both rich scopes now request `output_modalities=all`,
  preserving image, audio, embedding, and other model metadata. The regression
  verifies both query strings while preserving the legacy model-ID fetch URL.
  The default is documented in the [OpenRouter models guide](https://openrouter.ai/docs/guides/overview/models).
- Final authenticated live probe passed: 692 public models and 689 account-filtered
  models, each with the same number of retained pricing and architecture records;
  provider discovery returned one endpoint for `recursive/flint-v1.0:free`.
  Counts are a snapshot, not fixed regression expectations. This exercises actual
  upstream production-fetch helpers, not the gateway's deployed login/session or
  routing middleware. No inference requests, price imports, or billing operations
  were performed.
- Reproduce with
  `TEST_OPENROUTER_KEY_FILE=/private/tmp/new-api-openrouter-api-key GOCACHE=/private/tmp/new-api-openrouter-gocache go test -v ./controller -run '^TestOpenRouterAuthenticatedCatalogLive$' -count=1 -timeout=90s`.
  Evidence: `/private/tmp/new-api-phase2-authenticated-live.log` (exit 0).
  The key-info and invalid-key checks use the documented
  [key endpoint](https://openrouter.ai/docs/api-reference/limits).
- Re-ran the focused OpenRouter/ListModels command above after the completeness
  fix: all three packages passed (exit 0, about eight seconds). Evidence:
  `/private/tmp/new-api-phase2-focused-final.log`. The full-root runs above predate
  this narrow query/test change and were not repeated.
- Independent follow-up working-tree review approved current nine-file code/test
  fingerprint `ede71b542d584155a6af33a445268a1912e75b02` with no actionable findings.
  Whitespace checks passed.

### Returned-cost discovery and administration follow-up (2026-10-22)

- [x] Discover models without local prices when every route available to the
  requesting groups uses standard OpenRouter returned-cost billing and has an
  enabled credential. Preserve token model restrictions, effective Auto groups,
  and each group's exact-name/normalized-name routing precedence. Mixed local
  and returned-cost routes retain the local-price requirement. Existing priced
  models and the wallet/subscription settlement path are unchanged.
- [x] Add read-only billing source and channel-specific groups to model
  administration. Explain returned-cost billing beside local pricing; keep the
  billing-source selector in the channel editor. Translate the copy in all seven
  locales. No database schema, price entry, or billing mode is added.
- [x] Extend the existing model-list regression suite with returned-cost-only,
  local/mixed/enterprise, missing or disabled keys, disabled routes, token and
  Auto-group filtering, model-name fallback, source changes, and polling-cursor
  preservation. Verify source/group metadata without exposing channel secrets.
- [x] Real database checks passed without skips on SQLite 3.50.4, MySQL 8.0.46,
  and PostgreSQL 15.19, with the channel cache enabled and disabled. Command:
  `python3 /private/tmp/new-api-returned-discovery-matrix.py`; the wrapper runs
  `go test -v -p 2 ./controller ./model -run '^(TestReturnedCostModelDiscoveryDatabaseMatrix|TestListModels.*|TestGetUserModels.*|TestModelManagementDatabaseMatrix|TestPricingOpenRouterEndpointTypesDatabaseMatrix)$' -count=1 -timeout=8m`
  against isolated databases, then removes its containers and credentials.
  Evidence: `/private/tmp/new-api-returned-discovery/matrix.log`.
- [x] Frontend model-listing tests passed (28 cases), followed by
  `bun run typecheck`, oxlint on the four changed TypeScript/TSX files,
  `bun run i18n:sync`, and `bun run build`. Evidence:
  `/private/tmp/new-api-returned-discovery-web-{tests,typecheck,lint,build}.log`.
- [x] Independent working-tree review approved the fixes for per-group model-name
  fallback and a cached-channel polling-cursor read race. This is not staged
  review or commit approval. No race-detector run, live OpenRouter inference, or
  full `go test ./...` run was performed for this follow-up.
- [x] Inspect the rendered desktop/mobile channel-source and pricing states.
  Local Chromium with mocked management APIs verified per-channel sources and
  groups, keyboard pricing-tab navigation, mixed/returned-cost-only/local-only
  guidance, and contained drawer width. Visual review approved the additions.
  Command:
  `python3 /private/tmp/new-api-openrouter-cost-browser-runner.py /private/tmp/new-api-returned-discovery-ui.mjs`.
  Evidence: `/private/tmp/new-api-returned-discovery-ui.log` and
  `/private/tmp/new-api-returned-discovery-*-{connections,pricing}.png`.
  The existing mobile local-pricing mode tabs have crowded legacy labels in
  both local-only and returned-cost screenshots; that separate editor layout
  was not changed in this follow-up.
- Authorization scope: existing group and token restrictions remain server-side
  and fail closed for unpriced mixed routes, following the
  [OWASP Authorization Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html).
  Authentication/session flows and credential-management endpoints are unchanged.

## Phase 3: Pricing import through the existing UI

Deferred at the user's request; returned-cost discovery above is independent of
catalog-price import.

- [ ] Extend the existing pricing-sync preview and save flow rather than replacing
  it. Preserve response contracts, conflict checks, and administrator overrides.
- [ ] Distinguish catalog prices, provider endpoint prices, actual upstream cost,
  and the price charged to a new-api user.
- [ ] Parse prices with their actual units; do not treat every pricing field as
  USD per token. Preserve missing values versus explicit zero and reject invalid
  or sentinel values as importable prices.
- [ ] Generate self-contained billing-expression drafts for supported pricing,
  including applicable cache categories and context thresholds. Flag categories
  that cannot be represented or measured accurately instead of silently omitting
  charges or replacing existing prices.
- [ ] Highlight shared model-name impacts before applying imported prices. Do not
  silently migrate existing legacy configurations or follow live prices during
  settlement; freeze the selected billing configuration for each request.
- [ ] Reuse existing UI components and workflows for any metadata presentation;
  support all frontend locales and inspect rendered changes.

## Validation and completion criteria

- [ ] Add focused regressions for protocol paths, routing-field preservation,
  model mappings, tool calls, thinking, streaming completion/errors, and existing
  enterprise behavior.
- [ ] Verify native Messages cache/token semantics, zero/missing usage, interrupted
  streams, billing bounds, settlement/refunds, and legacy OpenRouter billing.
- [x] Cover catalog refresh failures, stale data, and provider-specific metadata.
- [ ] Cover pricing import units, tiers, explicit zero, overrides, and conflicts.
- [ ] Exercise existing channel model fetching and pricing preview/save behavior;
  run affected frontend tests, type checking, lint, and build for frontend changes.
- [x] Run focused backend tests. If relaykit changes, also run
  `GOWORK=off go build ./...` inside `relaykit/`.
- [x] If database behavior changes, run the required real SQLite/MySQL/PostgreSQL
  matrix, including migration checks where applicable.
- [x] Exercise authenticated upstream inference for affected formats when test
  credentials are available. Report live-test blockers and proof boundaries.
- [x] Obtain independent review before any requested behavioral-change commit.

### Precommit verification (2026-10-21)

- Independent staged review approved the adaptor/native Messages milestone with
  no actionable findings. Reviewed behavioral diff fingerprint:
  `dcbfa215e905fc0b0646e15359aa566fbdce2dde`.
- Re-ran the focused Go command above with `-count=1`, the standalone relaykit
  build, frontend typecheck, the three frontend test files (17 tests), and the
  targeted oxlint command; all exited 0. These checks used the working tree
  matching the staged source. No live or database checks were repeated.
- All staged Go files are gofmt-clean. Cached whitespace checks passed; all seven
  locale files contain exactly two added keys with no changed existing values.
- At the native Messages milestone, full `go test ./...` and live gateway
  accounting remained unverified. The returned-cost milestone above records the
  subsequent full-suite exception and authenticated gateway settlement check.

The initial assessment covered source inspection and public OpenRouter metadata.
The adaptor foundation, native Messages and returned-cost wallet billing
subsequently passed the focused local, database, frontend and live checks above.
The full-root test exception is recorded in the returned-cost milestone.

## Upstream references

- [Anthropic Messages](https://openrouter.ai/docs/api/api-reference/anthropic-messages/create-a-message)
- [Provider routing](https://openrouter.ai/docs/guides/routing/provider-selection)
- [Model catalog](https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties)
- [Account-filtered discovery](https://openrouter.ai/docs/api/api-reference/models/list-models-filtered-by-user-provider-preferences-privacy-settings-and-guardrails)
- [Provider endpoint records](https://openrouter.ai/docs/api/api-reference/endpoints/list-all-endpoints-for-a-model)
