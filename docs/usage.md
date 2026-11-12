# Using the planport API

For applications that call planport. Server setup: [README](../README.md).

planport speaks **OpenAI chat completions** plus one required field,
`provider`. It calls exactly the account and model you name and returns their
errors: no fallback, no model rewriting, no defaults.

## Connect

| | |
|---|---|
| base URL | `http://<PLANPORT_HOST>:<PLANPORT_PORT>/v1`, e.g. `http://127.0.0.1:18080/v1` |
| auth | when the server sets `PLANPORT_API_KEY`, network calls send `Authorization: Bearer <key>` (401 otherwise); same-machine calls via `127.0.0.1` / `localhost` need none. One key covers every provider |
| `provider` | required; an account from `GET /v1/providers`. Missing → 400, unknown → 404 listing valid names |
| `model` | required; passed to the account unchecked |

Call from programs. Browser requests (`Origin`, or `Sec-Fetch-Site` other than
`none` — including extensions, web-based API clients, Electron UIs) get 403.

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:18080/v1", api_key="<PLANPORT_API_KEY or anything>")
r = client.chat.completions.create(model="<model>", messages=[{"role": "user", "content": "Hi"}],
                                   extra_body={"provider": "<provider>"})
print(r.choices[0].message.content)
```

## Endpoints

| route | |
|---|---|
| `POST /v1/chat/completions` | chat, plain or `"stream": true`; requires `provider`, `model`, `messages` |
| `GET /v1/providers` | configured accounts with `models_supported`; no upstream call |
| `GET /v1/models?provider=<name>` | the account's models (`openai_compat` only; others → 501) |
| `GET /healthz` | `ok` |

## Chat

```bash
curl -s http://127.0.0.1:18080/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "provider": "<provider>", "model": "<model>",
  "messages": [{"role": "system", "content": "Answer briefly."}, {"role": "user", "content": "1+1=?"}]}'
```

Standard OpenAI response: `choices[0].message.content`, `usage`,
`finish_reason` (`"stop"`, or `"length"` / `"content_filter"` when the backend
cut the answer short).

- **No history is kept**: send the whole conversation each time, earlier answers
  as `assistant` messages (resent images and files are seen again).
- Several questions in one turn: ask for "all N answers, numbered".
- `reasoning_effort` (optional) is passed to the backend; valid values depend on
  the model (e.g. `none` `minimal` `low` `medium` `high` `xhigh`), a bad one is 400.

## Streaming

With `"stream": true` (`curl -N`) chunks arrive as OpenAI SSE: a `role` delta,
`content` deltas, a chunk with `finish_reason`, then `data: [DONE]`. An error
before the first token is a real HTTP status; an error after it ends the stream
with `finish_reason: "error"`. Streams carry no `usage`.

## Images and files

A `user` message's `content` may be an array mixing any number of parts:

| part | shape |
|---|---|
| text | `{"type": "text", "text": "..."}` |
| image | `{"type": "image_url", "image_url": {"url": "data:image/png;base64,<...>"}}` or an `https://` link |
| file | `{"type": "file", "file": {"filename": "a.pdf", "file_data": "data:application/pdf;base64,<...>"}}` (no `file_id`) |

Put each question right before the image or file it is about. Images and files
belong in `user` messages (`codex` and `claude_cli` reject them elsewhere;
`openai_compat` forwards as-is). Bodies are capped at 64 MiB (413); large images
cost many tokens, so shrink them. At most 2 bodies over 8 MiB (or sent without
`Content-Length`) are handled at once; later ones wait their turn. A body not
received within 5 minutes gets 408. Send local files through stdin to avoid the
shell's argument size limit:

```bash
python3 - "What is in each file?" photo.png contract.pdf <<'EOF' |
import sys, json, base64, mimetypes
question, files = sys.argv[1], sys.argv[2:]
parts = [{"type": "text", "text": question}]
for f in files:
    mt = mimetypes.guess_type(f)[0] or "application/octet-stream"
    url = f"data:{mt};base64," + base64.b64encode(open(f, "rb").read()).decode()
    if mt.startswith("image/"):
        parts.append({"type": "image_url", "image_url": {"url": url}})
    else:
        parts.append({"type": "file", "file": {"filename": f.split("/")[-1], "file_data": url}})
print(json.dumps({"provider": "<provider>", "model": "<model>", "messages": [{"role": "user", "content": parts}]}))
EOF
curl -s http://127.0.0.1:18080/v1/chat/completions -H 'Content-Type: application/json' -d @- | jq -r '.choices[0].message.content // .'
```

Verified on `codex`: PNG, JPEG, WebP, GIF, text and scanned PDF, TXT, CSV, DOCX,
XLSX, image links, and 7 questions with 2 images and 5 files in one message.

## Output

Always **one text string** — no images or binary files. Ask for file contents as
text (CSV, JSON, SVG, code), one fenced block per file, and save them yourself.

## Backend differences

| | `codex` | `openai_compat` | `claude_cli` |
|---|---|---|---|
| roles | `system`, `developer`, `user`, `assistant`; others → 400 | `role` + `content` forwarded | history packed into one transcript; images/PDFs only in the latest message |
| images | data URI, link | vendor-dependent | PNG/JPEG/GIF/WebP data URI |
| files | PDF, TXT, CSV, DOCX, XLSX… | vendor-dependent (some reject `file` parts) | UTF-8 text / JSON, PDF |
| `max_tokens` | ignored | forwarded | 400 |
| `reasoning_effort` | forwarded | forwarded | `low` `medium` `high` `xhigh` `max` only |
| `GET /v1/models` | 501 | listed | 501 |

## Ignored fields

Accepted but without effect on `codex`: `temperature`, `top_p`, `tools` (never
`tool_calls`), `response_format` (ask for JSON in the prompt), `max_tokens`. Not
supported: embeddings, `n` > 1, `logprobs`. `openai_compat` forwards only
`model`, `messages` (`role` + `content`), `max_tokens`, `reasoning_effort`.

## Errors

Bodies are `{"error": {"message": "…", "type": "…"}}`.

| HTTP | `type` | meaning | then |
|---|---|---|---|
| 400 | `invalid_request_error` | bad JSON or trailing data, missing field, unsupported role or part, bad `reasoning_effort`, model rejected with a backend 400 | fix the request |
| 401 | `auth` | network call with a missing or wrong key | fix the key |
| 403 | `forbidden` | browser request, or non-loopback `Host` without a server key | call from a program |
| 404 | `not_found` | unknown provider (valid ones listed) or route | fix the name / URL |
| 405 | `invalid_request_error` | wrong method | |
| 408 | `invalid_request_error` | request body not received within 5 minutes | send it faster or smaller |
| 413 | `invalid_request_error` | body over 64 MiB | shrink or split |
| 429 | `rate_limit_error` | account hit its usage limit | wait `Retry-After` seconds (when sent) or use another provider |
| 501 | `not_implemented` | model listing on a type that cannot list | |
| 502 | `upstream_error` | backend or network failure, broken credentials, timeout, other upstream status (e.g. 404 unknown model, 5xx) | retry if transient; else give the request id to the operator |

Messages hide internals: a timeout names `PLANPORT_TIMEOUT`; failures that are
not an upstream HTTP error (credential file, token refresh, connection) give only
the provider and a request id, with details in the server log; an upstream
401/403 is reported without its body. Every other upstream HTTP error (400, 429,
404 model, 5xx…) keeps the upstream's message.
