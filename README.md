# ccgw — a local LLM gateway for Claude Code

[![CI](https://github.com/bfreis/claude-code-llm-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/bfreis/claude-code-llm-gateway/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

A small Go server that sits on loopback, speaks the Anthropic Messages API, and
routes each request by model ID:

- **Claude models** are proxied to `api.anthropic.com` **byte-for-byte**, with the
  credential Claude Code sent relayed untouched — so a Claude subscription keeps
  being the thing that pays for them.
- **ChatGPT/Codex models** go to the ChatGPT Responses endpoint on your ChatGPT
  subscription, using the same OAuth sign-in the official Codex CLI uses.
- **OpenAI and OpenAI-compatible models** are translated in both directions.
- Everything is translated in-process — streaming, tool calls and reasoning
  included. No second proxy, no API key unless you want one.
- Configured models show up in Claude Code's `/model` picker.

```
claude ──ANTHROPIC_BASE_URL──▶ ccgw ──┬──▶ api.anthropic.com      Claude subscription
                                      ├──▶ chatgpt.com/…/codex    ChatGPT subscription
                                      ├──▶ api.openai.com/v1      OpenAI API key
                                      └──▶ any Messages-API endpoint
```

No other process, no API key required: ccgw speaks each protocol itself.

## Quick start

```sh
go build -o ccgw ./cmd/ccgw
./ccgw setup                      # detects what is installed and writes the config
./ccgw serve                      # also writes Claude Code's model-picker cache
```

`setup` reads what is already on the machine rather than asking: whether Claude
Code is signed in and with what, whether an `ANTHROPIC_API_KEY` is quietly
overriding your subscription, whether the Codex CLI is installed and signed in,
which version it reports (the backend gates model availability on that), and
which model its own `config.toml` names. On a machine where both CLIs already
work, the only question is whether to write the file — and `-y` skips that too.
If there is no ChatGPT sign-in, it offers to open a browser and do it.

`ccgw init` still writes a commented starter config non-interactively.

In another shell:

```sh
eval "$(./ccgw env)"              # fidelity mode; see the three modes below
claude
```

`./ccgw models` prints the catalogue and the IDs Claude Code will see.
After changing the model list, re-run `./ccgw sync-picker` and give Claude Code
a full restart — the picker is read once, at startup.

## The three launch modes

`ccgw env -mode <mode>` prints the environment for each. They differ in what
Claude Code sends, which is measurable — the table below was taken off the wire
against Claude Code 2.1.273 with the same prompt in each mode.

| | `fidelity` (default) | `discovery` | `gateway` |
|---|---|---|---|
| models in `/model` picker | **yes** (from the cache) | yes (fetched) | yes (fetched) |
| `anthropic-beta` values | **11** | 9 | 4 |
| prompt cache `ttl` | **1 h** | 5 min (default) | 5 min (default) |
| `cache_control` breakpoints | 3 | 3 | 3 |
| `context_management` | **yes** | yes | no |
| credential Claude Code sends | its own OAuth token | `ANTHROPIC_AUTH_TOKEN` | `ANTHROPIC_AUTH_TOKEN` |
| `oauth-2025-04-20` beta | sent by Claude Code | **re-added by ccgw** | **re-added by ccgw** |

### `fidelity` — recommended, nothing given up

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
```

Claude Code sends its own credential (subscription OAuth token, or
`ANTHROPIC_API_KEY` if set), its full beta set and the 1 h cache TTL. Claude
models behave exactly as they would without the gateway in the path.

The picker rows come from the discovery cache the gateway writes — see below.
No extra credential is needed, which is precisely why nothing is lost.

### `discovery` — Claude Code fetches the list itself

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
export ANTHROPIC_AUTH_TOKEN=<token from `claude setup-token`>
unset ANTHROPIC_API_KEY
```

Claude Code fetches `GET {base}/v1/models?limit=1000` at startup, so the picker
self-updates without re-running `sync-picker`. That convenience has a price:
`ANTHROPIC_AUTH_TOKEN` becomes the credential Claude Code sends, which makes it
omit `ttl` from the `cache_control` blocks — entries then live 5 minutes instead
of an hour — and drop two betas. Make the token a real subscription token
(`claude setup-token`); with `anthropic.auth: passthrough` the gateway relays it
to Anthropic and adds back the `oauth-2025-04-20` beta Claude Code stops sending
once a token arrives this way, without which Anthropic rejects the credential.

Prefer this only if you run on an API key, where the cache TTL is moot.

### How the picker rows get there without a credential

Claude Code will fetch `/v1/models` itself, but only when it has an
`ANTHROPIC_AUTH_TOKEN`, an `apiKeyHelper` or an API key — a plain subscription
OAuth session does not qualify and it logs `[gatewayDiscovery] skipped: no
credential`. Satisfying that check is what costs `fidelity` mode its cache TTL.

Its *read* path has no such requirement:

```js
function tAe(){ if(!Vg()) return [];
  let e = hK(Wg());
  if(!e || e.baseUrl !== a.ANTHROPIC_BASE_URL) return []; … }

function Vg(){ if(!a.CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY) return false;
  if(Pe()!=="firstParty") return false; if(Ko()) return false;
  if(!a.ANTHROPIC_BASE_URL) return false; return true }
```

So `ccgw serve` writes that cache itself, at
`$CLAUDE_CONFIG_DIR/cache/gateway-models.json` (default `~/.claude/cache/…`),
mode 0600:

```json
{ "baseUrl": "http://127.0.0.1:8787",
  "fetchedAt": 1789539348903,
  "models": [{"id": "anthropic/gpt-5.6", "display_name": "GPT-5.6", "description": "…"}] }
```

- `baseUrl` must equal `ANTHROPIC_BASE_URL` **exactly** — the reader compares
  strings, and silently ignores a mismatch. Both are derived from `listen`.
- `ccgw sync-picker` writes it on demand; `-remove` deletes it. `ccgw serve
  -no-picker-sync` turns the automatic write off.
- Claude Code reads it **only at startup**, so a full restart is required after
  changing the model list. Reloading plugins is not enough.
- At most 100 models are kept.

You can always bypass the picker and name a model directly, in any mode:

```sh
claude --model anthropic/gpt-5.6
# or
export ANTHROPIC_MODEL=anthropic/gpt-5.6
```

Claude Code prints a harmless `[claude-code:unrecognized_model]` warning for an
ID it does not know, then sends the request anyway.

### `gateway` — kept for completeness

```sh
export CLAUDE_CODE_USE_GATEWAY=1
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_AUTH_TOKEN=<token>
```

This is Claude Code's enterprise inference-gateway mode. It reaches the picker
through a different code path (`[Bootstrap] Gateway /v1/models` rather than
`[gatewayDiscovery]`) and sends the smallest request of the three: 4 betas, no
`context_management`, no cache TTL. **`fidelity` does the same job while giving
up nothing — prefer it.** Plain HTTP on loopback is accepted in both, with no
TLS pin and no consent prompt.

Gateway mode **adds to** the picker rather than replacing it: the built-in Claude
models stay, and discovered models are appended and de-duplicated. The `mythos`
family has no gateway ID and so does not appear, and discovery has a hardcoded
5 s timeout with no env override (on timeout the previously cached list is kept).

### Why `add_betas` cannot close these gaps

Each dropped feature is gated inside Claude Code on a request *body* field, and
the request builder only emits the field when its own beta set contains the
beta — a decision made before the request leaves the process. A header added
downstream arrives too late. For example `context_management` is built by:

```js
function Isr(e){ let {hasThinking:n=!1}=e??{};
  if(n) return {edits:[{type:"clear_thinking_20251015", keep:"all"}]};
  return }
```

and attached only under `...kP && ka && Ks.includes(fKe) && {context_management:kP}`.
The one genuinely header-only beta is `oauth-2025-04-20`, which the gateway
handles automatically. Use `add_betas` only for a beta you know is header-only.

## Model naming

Claude Code's discovery filters the `/v1/models` response, and only IDs matching
`/(claude|anthropic)/i` survive. A bare `gpt-5.6` is silently dropped.

`ccgw` therefore advertises non-Anthropic models under an `alias_prefix`
(default `anthropic/`), and strips it again before calling the provider:

| config `id` | advertised to Claude Code | sent to the provider |
|---|---|---|
| `gpt-5.6` | `anthropic/gpt-5.6` | `gpt-5.6` |

An ID that already contains `claude` or `anthropic` is left alone. Set
`alias_prefix: ""` to disable prefixing entirely — `ccgw models` and `ccgw serve`
warn about any ID that would then be dropped.

There is a second half to the filter. Claude Code also drops an ID that is an
**exact, case-insensitive match** for one of the provider spellings of a model in
its own baked catalog — the 19 rows covering every Claude family, each carrying
eight spellings (first-party, Bedrock, Vertex, Foundry and so on). The match is
exact: no prefix matching, no date stripping. The sole exception is the `fable`
family, which is allowed through.

The practical consequence is small — those models are already in the picker
natively, so there is nothing to add — and the default `anthropic/` prefix makes
an exact collision impossible. It only bites if you turn prefixing off and name a
model exactly like a real Claude ID.

## Configuration

```yaml
listen: 127.0.0.1:8787
alias_prefix: "anthropic/"

anthropic:
  base_url: https://api.anthropic.com
  auth: passthrough          # passthrough | bearer | api_key
  # token_env: CCGW_ANTHROPIC_TOKEN    # with auth: bearer
  # api_key_env: ANTHROPIC_API_KEY     # with auth: api_key
  # Only useful for header-only betas; see the note in the gateway-mode
  # section. oauth-2025-04-20 is added automatically when needed.
  add_betas: []

providers:
  - name: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
    # headers: {X-Org: acme}
    # reasoning: thinking                     # thinking (default) | drop
    # max_tokens_field: max_completion_tokens # or max_tokens for older APIs
    # drop_temperature: false                 # true for reasoning models
    # max_tokens_cap: 0                       # clamp the output cap
    # keep_plan_tools: false                  # see below

models:
  - id: gpt-5.6
    provider: openai
    display_name: GPT-5.6
    description: OpenAI GPT-5.6
    # max_tokens: 32000      # clamp the output cap for this model
    # long_context: true     # advertise as gpt-5.6[1m]; see below
```

`long_context` appends a `[1m]` suffix to the advertised ID. Claude Code assumes
a 200k window for a model it does not recognise, and reads that suffix as a
1M client-side window; the gateway strips it again before calling the provider.
It is a claim you are making about the backend, not a request to it — only set
it for a model that really accepts that much input, and consider an explicit
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` if the true limit is lower.

Any model ID not listed under `models` falls through to Anthropic, so the Claude
models never need enumerating.

`base_url` works with anything that speaks OpenAI Chat Completions — OpenRouter,
Groq, Together, vLLM, LM Studio, Ollama (`http://localhost:11434/v1`).

## HTTP surface

| Route | Purpose |
|---|---|
| `HEAD/GET /api/hello` | Claude Code's reachability probe. It sends this before using the base URL at all. |
| `GET /v1/models` | The catalogue, for gateway-mode discovery. |
| `POST /v1/messages` | Inference. Routed by the `model` field. |
| `POST /v1/messages/count_tokens` | Proxied for Claude models; estimated for provider models. |
| everything else | Proxied to Anthropic unchanged. |

That last row matters: an endpoint this gateway has never heard of still works.

`count_tokens` is called in three situations — sizing an MCP tool result that
looks larger than half of `MAX_MCP_OUTPUT_TOKENS` (25,000 by default), running
`/context`, and sizing a skill — and **every caller falls back to its own local
estimate if the call fails**. So the estimate the gateway returns for provider
models is safe: it counts the text content of the request rather than the raw
JSON, excluding base64 image payloads, which would otherwise make every
screenshot look like an oversized prompt.

## Translation notes

Anthropic → OpenAI:

- `system` (string or blocks) becomes a leading `system` message.
- `tool_result` blocks become `role: tool` messages, emitted **before** the rest
  of the user message so they follow the assistant `tool_calls` they answer.
- `tool_use` becomes `tool_calls`; `input_schema` becomes `function.parameters`.
- Images become `image_url` parts with a `data:` URI.
- `thinking` blocks are dropped — they are signed by the model that produced
  them and cannot be replayed to another provider.
- `thinking.budget_tokens` maps to `reasoning_effort` (low / medium / high).
- The output cap is sent as `max_completion_tokens` (`max_tokens_field` switches
  it back for older APIs).
- `EnterPlanMode` / `ExitPlanMode` are withheld. They drive Claude Code's own
  plan/accept workflow rather than doing work, and a non-Claude model handed
  them calls them unprompted and stalls the turn. `keep_plan_tools: true`
  forwards them anyway.

### Switching between a provider model and Claude mid-session

The gateway turns a provider's reasoning into Anthropic `thinking` blocks, which
have no Anthropic signature. When Claude Code then replays that history to a
Claude model, Anthropic rejects it:

```
400 … Invalid `signature` in `thinking` block
```

Claude Code recovers — `[thinking] server rejected a thinking block; stripping
all thinking blocks and retrying` — but it costs a round trip and discards
Claude's *own* signed reasoning along with the offending block. So the gateway
removes unsigned thinking blocks on the way to Anthropic, and only those: signed
blocks and `redacted_thinking` pass through, and a body with nothing to strip is
forwarded byte-identical. Measured before and after on a real session, the first
Claude turn after a provider turn went from `400` + retry to a single `200`.

`reasoning: drop` avoids creating them in the first place, at the cost of not
seeing the model reason.

The same mechanism carries Codex's encrypted reasoning state: a `ccgw:`-prefixed
signature is how the gateway recognises a thinking block it minted, whether it
holds a backend's reasoning blob or nothing at all. Anthropic never issued those
signatures, so they are removed on the way to Anthropic and decoded on the way
back to the backend that understands them.

OpenAI → Anthropic:

- Streaming deltas are re-framed into `content_block_start` / `_delta` / `_stop`
  with sequential indices; each tool call gets its own block, and late argument
  fragments are routed back to the block that owns them.
- `reasoning_content` / `reasoning` become `thinking` blocks.
- `finish_reason` maps onto `stop_reason`; `prompt_tokens_details.cached_tokens`
  maps onto `cache_read_input_tokens`.
- Truncated tool arguments are replaced with `{}` rather than emitted as invalid
  JSON.
- Errors keep the upstream status and are wrapped in Anthropic's
  `{"type":"error","error":{"type":...,"message":...}}` envelope. The `type` is
  always translated into Anthropic's vocabulary (`rate_limit_error`,
  `overloaded_error`, `invalid_request_error`, …) because that is the field
  Claude Code classifies on; the provider's own name for the condition is kept
  in `message`, where it is useful to a human but harmless to the client.
- `retry-after` is clamped to 60 s. Claude Code treats the header as a floor on
  its backoff and **aborts the turn** if the resulting delay would exceed 60 s,
  so forwarding a provider's `retry-after: 3600` verbatim would turn a
  retryable rate-limit into a dead turn. HTTP-date values are dropped, since
  Claude Code parses the header with `parseInt`.

### Timing budget

Against a custom `ANTHROPIC_BASE_URL`, Claude Code allows:

- **600 s** to the response headers (`API_TIMEOUT_MS`, which bounds time-to-headers
  only, not the stream).
- **300 s** between SSE events (`CLAUDE_STREAM_IDLE_TIMEOUT_MS`, which the env var
  can only raise), with a user-visible warning at 150 s.

The byte-level and first-byte watchdogs are **off** for a custom base URL — both
require the host to be `api.anthropic.com` — so the familiar "a proxy or gateway
that buffers streaming responses can cause this" message cannot fire here. The
event-level idle watchdog is the one that applies, and it is on by default.

The gateway emits an SSE `ping` every 60 s of silence so that a model which
reasons for minutes before its first token does not trip the 300 s abort. `ping`
events are accepted anywhere in the stream, including before `message_start`
(verified against Claude Code 2.1.273).

## Development

```sh
go test ./...
go vet ./...
```

### Verifying the picker

The picker is the one behaviour no unit test can reach: it lives inside Claude
Code, is driven by the cache file above, and only renders in a real terminal.
`scripts/verify-picker.py` drives the actual TUI in a pty, opens `/model`, and
reports whether a row is present. Run it from a directory Claude Code trusts,
with the gateway already serving:

```sh
./ccgw sync-picker
python3 scripts/verify-picker.py http://127.0.0.1:8787 "GPT-5.6"   # exit 0

./ccgw sync-picker -remove
python3 scripts/verify-picker.py http://127.0.0.1:8787 "GPT-5.6"   # exit 1
```

**Run both.** A row present in the first run proves nothing on its own — only
its disappearance in the second shows it came from the cache. That pair is how
this feature was verified: with the cache the picker listed

```
  6. Haiku              Haiku 4.5 · Fastest for quick answers
  7. GPT-5.6 (fake)     fake OpenAI backend
```

and without it the list ended at row 6, everything else unchanged.

## Using a ChatGPT/Codex subscription

A ChatGPT Plus/Pro subscription is not an OpenAI API key and is not reachable at
`api.openai.com`. `type: codex` talks to it the way the official Codex CLI does —
OAuth credentials, the ChatGPT Responses endpoint, translation performed here.

```sh
ccgw codex login     # browser OAuth against your ChatGPT account
ccgw codex status    # shows the plan, account and token expiry
```

`ccgw setup` writes this for you, configuring the whole known catalogue —
Astra, Sol, Terra and Luna — plus whatever model your Codex CLI is set to if
that is something else:

```yaml
providers:
  - name: codex
    type: codex
    client_version: "0.155.0-alpha.11"   # taken from `codex --version`
    # service_tier: priority             # the faster tier
    # reasoning: thinking                # thinking (default) | drop

models:
  - id: "gpt-6-astra"
    provider: codex
    display_name: "GPT-6 Astra (Codex)"
  - id: "gpt-5.6-sol"
    provider: codex
    display_name: "GPT-5.6 Sol (Codex)"
  - id: "gpt-5.6-terra"
    provider: codex
    display_name: "GPT-5.6 Terra (Codex)"
  - id: "gpt-5.6-luna"
    provider: codex
    display_name: "GPT-5.6 Luna (Codex)"
```

Whether your account serves all of them is between you and OpenAI: a model it
does not serve is refused when selected, not when configured, so an extra row
costs nothing until you pick it.

If you already use the Codex CLI, its credential is reused as-is — the default
path is `$CODEX_HOME/auth.json`, else `~/.codex/auth.json`, and `auth_path`
overrides it. `ccgw codex login` writes the same file, merging rather than
replacing so the CLI keeps whatever else it stored there. Either way, the access
token is refreshed automatically five minutes before it expires; only a dead
grant asks you to sign in again.

### If a model is refused: `client_version`

The backend gates model availability on the Codex client version it is told, and
refuses anything newer than that client supports:

```
400 codex: The 'gpt-5.6-sol' model requires a newer version of Codex.
    Please upgrade to the latest app or CLI and try again.
```

ccgw reports a real Codex release by default, but that pins to whatever was
current when the gateway was built and the model list moves faster. Set
`client_version` to what `codex --version` prints on the same machine — no
rebuild needed, and no need to wait for ccgw to catch up.

### Reasoning survives across turns, without a session store

The Responses API returns reasoning as an opaque encrypted blob that should be
replayed on the next turn to preserve the model's chain of thought. ccgw is
stateless — it rebuilds each request from the conversation Claude Code replays —
so there is nowhere to keep that blob.

It rides out instead in the `signature` field of the Anthropic thinking block,
which Claude Code returns verbatim on replay (measured against 2.1.273). On the
way back it is decoded into a `reasoning` item again. The consequence worth
knowing: those blocks carry a `ccgw:` signature Anthropic did not issue, so the
Anthropic passthrough strips them — see the note under Translation notes.

### What it sends

Flat function tools (`{"type":"function","name":…}`, no `function` wrapper) with
`strict:false`, `arguments` as a JSON-encoded string, tool results as bare
strings, `store:false`, and `include:["reasoning.encrypted_content"]`. Claude
Code's session id becomes the `session-id` and `prompt_cache_key`, which is what
gives the backend cache affinity across the turns of one conversation.

## Forwarding to another Messages-API endpoint

`type: anthropic-compatible` sends the request onward in the shape it already
has, letting the far end translate. Use it for anything that already speaks the
Anthropic Messages API — LiteLLM's Anthropic endpoint, a self-hosted gateway,
another ccgw:

```yaml
providers:
  - name: elsewhere
    type: anthropic-compatible
    base_url: http://127.0.0.1:8080

models:
  - id: some-model-id
    provider: elsewhere
    display_name: Some Model
```

It is also the right choice whenever a backend's OpenAI-compatible route cannot
carry tool calls, since Claude Code is entirely tool-driven.

What ccgw changes on that path, and nothing else:

- the model ID becomes the backend's own (the `anthropic/` prefix and any `[1m]`
  suffix are removed);
- `EnterPlanMode` / `ExitPlanMode` are withheld (`keep_plan_tools: true` keeps
  them);
- your Anthropic credentials are **stripped** — `authorization`, `x-api-key`,
  `proxy-authorization` and `cookie` never reach the backend, which serves a
  different provider entirely;
- `count_tokens` is forwarded rather than estimated, because the backend
  tokenizes properly.

The translation options (`reasoning`, `max_tokens_field`, `drop_temperature`,
`max_tokens_cap`) do not apply here and are rejected rather than ignored — this
provider type does no translation.

## Known limits

- **Input context window.** Claude Code assumes 200k for a model it does not
  recognise. A provider model with a smaller window will overflow before Claude
  Code thinks to compact; one with a larger window is under-used unless you set
  `long_context`. The lever is `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, which the
  gateway does not set for you.
- **Tool schemas are forwarded as-is.** Some backends reject regex constructs
  that Claude Code's schemas contain — character-class shorthands of the
  `\p{...}` family are the ones seen in practice. ccgw neither checks for this
  nor rewrites around it, so a 400 naming a tool schema points here.
- **Images inside `tool_result` are flattened to text.** OpenAI's `tool` role
  takes a string, so an image returned by a tool is lost on that path. Images in
  user messages are forwarded.
- **No usage or cost accounting.** Token counts are relayed from the provider
  when it reports them; nothing is aggregated.
- **`count_tokens` for provider models is an estimate**, not a tokenizer count.
- **The gateway does not supervise itself.** It is a plain server you run; if it
  dies, Claude Code sees a connection error until you restart it.

## License

MIT. See [LICENSE](LICENSE).

## Acknowledgements

ccgw contains no code from the projects below. What it took from them are ideas,
and in one case the confidence that an approach works in practice. They are
named here because that debt is real and because anyone auditing this gateway's
wire behaviour will want the same sources.

**[openai/codex](https://github.com/openai/codex)** — Apache-2.0.
The Codex CLI is the reference client for the ChatGPT Responses endpoint that
`type: codex` talks to. Its source is where the OAuth constants, the credential
file layout and the request and streaming formats were read from, rather than
guessed; the citations in `internal/provider/codex/` point at the files each
constant came from.

**[raine/claude-code-proxy](https://github.com/raine/claude-code-proxy)** — MIT,
Copyright (c) 2026 Raine Virta.
Its behaviour settled a question no documentation answered: whether the Responses
endpoint accepts a request that replays no reasoning items. Because that proxy is
stateless and omits them routinely, the answer was evidently yes, which is what
made ccgw's stateless design safe to commit to. Carrying a backend's reasoning
state through the Anthropic thinking block's `signature` field is also its idea.

**[Eigenwise/eigenwise-toolshed](https://github.com/Eigenwise/eigenwise-toolshed)**
— MIT, Copyright (c) 2026 Eigenwise.
Its `model-gateway` plugin demonstrated that Claude Code's model-picker cache can
be written directly, which is what lets ccgw populate `/model` without the
credential that live discovery demands — and therefore without giving up the 1 h
prompt cache. That is the whole basis of `fidelity` mode.

## How the Claude Code side was determined

The behaviours above were established against Claude Code 2.1.273 by running it
against instrumented local servers, driving its TUI in a pty, and reading the
strings in its binary — not from documentation. They are version-specific;
worth re-checking after a Claude Code upgrade.
