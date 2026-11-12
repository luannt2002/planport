# planport

Your own subscription plans and API keys as a local OpenAI-compatible API.
planport serves `POST /v1/chat/completions`; any OpenAI client works once it
points at planport and adds a `provider` field. One Go binary, stdlib only.
How to call the API: [docs/usage.md](docs/usage.md).

> ⚠️ **Unofficial.** Using subscription tokens outside the official clients may
> break the vendor's terms of service and get the account limited or banned.
> Backends change without notice. Use at your own risk.

**Contract:** the client names a `provider` (one account) and a `model`;
planport calls exactly that account with exactly that model and returns its
errors. No fallback, no model rewriting, no defaults.

| provider type | backend | credentials |
|---|---|---|
| `codex` | ChatGPT Codex (Responses API, OAuth), ChatGPT Plus/Pro | auth file from `codex login` |
| `openai_compat` | any OpenAI chat-completions endpoint | file holding one API key |
| `claude_cli` | Claude Code CLI as a subprocess (experimental) | the logged-in CLI |

## Run

Go ≥ 1.22, Linux or macOS.

```bash
git clone https://github.com/luannt2002/planport && cd planport
go build -o bin/planport .
cp -n .env.example .env && chmod 600 .env    # fill in the 🔒 keys
./bin/planport                                # reads ./.env; -env <path> for another file

curl http://127.0.0.1:18080/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"provider": "codex", "model": "<model>", "messages": [{"role": "user", "content": "Hi"}]}'
```

Background: `(umask 077; setsid ./bin/planport > /tmp/planport.log 2>&1 &)` (the
log names credential files) · stop: `pkill -x planport`. SIGTERM finishes
in-flight requests (streams, token refreshes) before exiting; a second signal
exits at once.

## Configuration

All settings come from env / `.env` ([.env.example](.env.example)), read only in
`internal/config`. Provider `<N>` of type `<T>` resolves key `<K>` from
`PLANPORT_PROVIDER_<N>_<K>`, then `PLANPORT_TYPE_<T>_<K>`, then `PLANPORT_<K>`.

- Every key is required except `PLANPORT_API_KEY`; code has no defaults. Bad or
  missing values (URL, port, duration, host, type, duplicate provider, shared
  credential file) stop startup with the full list. A malformed or repeated
  `.env` line stops startup at that line. Env vars already set win over `.env`.
- 🔒 Secrets and vendor data (API key, credential paths, endpoints, client ID,
  `BETA`, `USER_AGENT`) live only in `.env` (git-ignored, mode 600); take them
  from the vendor's docs or the official CLI's source. `.env.example` holds
  project values only — a test enforces it. `USER_AGENT` should say
  `planport/<version>`.
- `CRED_FILE` is the file `codex login` writes, or a mode-600 file holding only
  the vendor's API key. A missing file is a startup warning; requests to that
  provider fail until it exists. Two providers may not share one (symlinks and
  hard links resolved).
- `PLANPORT_TIMEOUT` caps a whole upstream call: a non-streaming call then gets
  502; a stream that already sent text ends with `finish_reason: "error"`.
- More accounts or vendors need env only: add the name to `PLANPORT_PROVIDERS`
  with its `TYPE` and `CRED_FILE` (plus `BASE_URL` for a new `openai_compat`
  vendor).
- Fixed limits, not settings: at most 2 request bodies over 8 MiB (or without
  `Content-Length`) are handled at once, later ones wait; a body not received
  within 5 minutes gets 408.

## Network access

`.env.example` binds `127.0.0.1` (this machine only). To open it, set
`PLANPORT_HOST` to a machine IP or `0.0.0.0` **and** `PLANPORT_API_KEY`
(startup fails without it).

- Network calls send `Authorization: Bearer <key>` (401 otherwise). Calls from
  this machine (loopback connection and `Host`) need no key, so the key does not
  stop other users of the same machine.
- Browser requests (`Origin`, or `Sec-Fetch-Site` other than `none`) get 403;
  without a key, a non-loopback `Host` gets 403 too (DNS rebinding).
- A same-machine proxy in front of planport makes network calls look local and
  skip the key — don't. Likewise an SSH tunnel to `127.0.0.1` lets anyone who
  reaches the tunnel port in.
- The key travels over plain HTTP: open planport only on networks you trust.

## Tokens (`codex`)

Refresh tokens rotate, so two parties refreshing one token log one of them out.

- One credential file per provider and per planport process; the refresh lock is
  in-process only, so prefer accounts the Codex CLI is not using at the same time.
- planport refreshes when the access token (JWT `exp`) is within
  `REFRESH_MARGIN` and writes it back to the file it read (temp file, fsync,
  rename, directory fsync, mode 600).
- A refresh finishes even if the client cancels. A failed refresh falls back to
  the old token while it is valid, and the refresh is retried at most every
  30 s meanwhile (a 429/5xx does not ask for a new login). A token that cannot be
  saved stays in memory and is saved later — fix the disk before stopping
  planport.
- A rejected refresh token (400/401/403) is not resent until the file changes:
  run `codex login`.
- `openai_compat` keys are re-read from their file on every request.

## claude_cli (experimental)

Runs `claude -p` per request in a temp directory (tools, MCP, slash commands and
session persistence off) and converts its NDJSON to OpenAI. It never touches
credentials; the child gets no `ANTHROPIC_*`, `CLAUDE_CODE_*` or `PLANPORT_*`
variables. One CLI per provider at a time; waiting counts against
`PLANPORT_TIMEOUT`.

```dotenv
PLANPORT_PROVIDERS=claude
PLANPORT_PROVIDER_CLAUDE_TYPE=claude_cli
PLANPORT_PROVIDER_CLAUDE_BINARY=/path/to/claude   # command -v claude
```

## Layout

```
main.go                          -env flag, wiring, graceful shutdown
internal/config/                 env / .env loading and validation
internal/server/                 HTTP routes, auth, errors, SSE
internal/provider/               Provider interface, registry, errors, SSE reader
internal/provider/codex/         codex
internal/provider/openaicompat/  openai_compat
internal/provider/claudecli/     claude_cli
internal/tokenstore/             credential file, OAuth refresh
```

## Development

```bash
gofmt -l . && go vet ./... && go test -race ./...
PLANPORT_CLAUDE_LIVE=1 go test ./internal/provider/claudecli -run '^TestLiveClaudeHTTP$' -count=1 -v -timeout 9m   # real CLI, uses quota
```

License: [MIT](LICENSE).
