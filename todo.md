# OpenRouter support improvements

Recorded: 2026-10-21. Assessment baseline: `6cf0d90b5` on
`v1.0.0-rc.45-custom`. The dedicated adaptor foundation is implemented; the
remaining protocol, catalog, and pricing work is pending.

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
- [ ] Preserve documented OpenRouter routing fields without requiring blanket
  body passthrough. Define precedence between client options and administrator
  policy; preserve explicit zero and false values.
- [ ] Enforce authorization and valid pricing for every model allowed through
  multi-model fallback options before forwarding them.
- [ ] Retain request bounds, parameter overrides, reservations, settlement,
  refunds, and audit behavior across the new paths.
- [ ] Correct advertised endpoint capabilities to match verified runtime support.
- [ ] Check OpenRouter streaming usage support and update the stream-support
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
  nothing is staged or committed. Full `go test ./...`, deployed gateway
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

## Phase 2: Catalog and provider metadata

- [ ] Add a cached OpenRouter catalog service retaining capabilities, modalities,
  context/output limits, supported parameters, and raw pricing metadata.
- [ ] Keep existing model-fetch responses as arrays of model IDs; expose richer
  metadata through an additive API.
- [ ] Evaluate account-filtered discovery separately from the public catalog.
  Never expose models merely because OpenRouter lists them; preserve local
  channel enablement and user access controls.
- [ ] Fetch per-model provider endpoint details on demand, with bounded requests,
  timeouts, caching, timestamps, and last-successful-data fallback.
- [ ] Treat unavailable or null provider metrics as unknown. Do not automatically
  disable channels based on upstream catalog or health changes.

## Phase 3: Pricing import through the existing UI

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
- [ ] Cover catalog refresh failures, stale data, provider-specific metadata, and
  pricing import units, tiers, explicit zero, overrides, and conflicts.
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
- Full `go test ./...` and the live gateway proof boundaries above remain open.

The initial assessment covered source inspection and public OpenRouter metadata.
The adaptor foundation and native Messages subsequently passed the focused
local, database, frontend and live checks recorded above. Full `go test ./...`
remains unperformed.

## Upstream references

- [Anthropic Messages](https://openrouter.ai/docs/api/api-reference/anthropic-messages/create-a-message)
- [Provider routing](https://openrouter.ai/docs/guides/routing/provider-selection)
- [Model catalog](https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties)
- [Provider endpoint records](https://openrouter.ai/docs/api/api-reference/endpoints/list-all-endpoints-for-a-model)
