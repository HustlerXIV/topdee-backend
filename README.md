# Backend (Go)

Fiber HTTP API. Owns auth, tenants, knowledge bases, channel connections, conversation log, and the orchestrator that routes both playground messages and channel webhooks through the AI service.

## Run

```bash
cp .env.example .env
go mod tidy
go run .
```

Listens on `:8080`.

## Mental model

There is **one platform agent**, configured by env vars (`PLATFORM_SYSTEM_PROMPT`, `PLATFORM_MODEL`, `PLATFORM_TEMPERATURE`). Tenants don't see it. Each tenant only configures:

- Knowledge bases (uploaded files → chunks in Qdrant, scoped by `tenant_id`)
- Channel connections (Facebook page access token, LINE channel secret + token)

The orchestrator (`internal/handlers/chat.go`) is the single code path that answers a message — whether it came from `/api/v1/playground/chat` or from a Facebook webhook.

## Endpoints

| Method   | Path                                       | Auth  | Notes                                                            |
| -------- | ------------------------------------------ | ----- | ---------------------------------------------------------------- |
| GET      | `/health`                                  | —     | DB + AI service ping                                             |
| POST     | `/api/v1/auth/register`                    | —     | Creates tenant + owner user                                      |
| POST     | `/api/v1/auth/login`                       | —     | Returns JWT                                                      |
| GET      | `/api/v1/knowledge`                        | JWT   | List tenant's knowledge bases                                    |
| POST     | `/api/v1/knowledge`                        | JWT   | Create KB                                                        |
| GET      | `/api/v1/knowledge/:id`                    | JWT   | Get KB (with file list + chunk count)                            |
| DELETE   | `/api/v1/knowledge/:id`                    | JWT   | Delete KB; cascades vector delete via AI service                 |
| POST     | `/api/v1/knowledge/:id/files`              | JWT   | Multipart upload; backend proxies to AI service `/ingest/file`   |
| GET      | `/api/v1/channels`                         | JWT   | Connection status for FB + LINE (tokens redacted)                |
| PUT      | `/api/v1/channels/facebook`                | JWT   | Connect FB page (`page_id`, `page_name?`, `page_access_token`)   |
| DELETE   | `/api/v1/channels/facebook`                | JWT   | Disconnect FB                                                    |
| PUT      | `/api/v1/channels/line`                    | JWT   | Connect LINE (`channel_id`, `channel_secret`, `channel_access_token`) |
| DELETE   | `/api/v1/channels/line`                    | JWT   | Disconnect LINE                                                  |
| POST     | `/api/v1/playground/chat`                  | JWT   | Test the platform agent with this tenant's data                  |
| GET      | `/api/v1/playground/conversations/:id`     | JWT   | Message history                                                  |
| GET/POST | `/webhooks/facebook`                       | —     | GET: verify subscription; POST: HMAC-verify, route by `page_id`, reply via Send API |
| POST     | `/webhooks/line`                           | —     | Stub — signature verification + Reply API not yet wired          |

## Architecture

```
main.go
  └─ config       env loading (incl. PLATFORM_*, FB_APP_SECRET, FB_VERIFY_TOKEN)
  └─ db.Connect   MongoDB + ensure indexes (unique sparse on facebook.page_id)
  └─ clients      AI service client (chat, ingest/file, ingest/kb/delete)
  └─ Fiber router
       ├─ auth/        JWT issue/parse
       ├─ middleware/  RequireAuth → puts tenant_id in fiber context
       ├─ models/      Mongo BSON shapes (Tenant w/ Facebook+Line, KnowledgeBase, Message)
       └─ handlers/    auth · knowledge · channels · playground · webhooks
            └─ Orchestrator   shared by playground + webhooks
```

### Tenant isolation

- Every authenticated handler reads `tenant_id` from JWT claims (`middleware.TenantID(c)`) and includes it in every Mongo query.
- The orchestrator passes `tenant_id` to the AI service, which adds it as a Qdrant payload filter on every vector search.
- The Facebook webhook routes to a tenant by looking up `tenants.facebook.page_id`. A unique sparse index prevents two tenants from claiming the same FB page; `ConnectFacebook` returns 409 on duplicate.

### Channel access tokens

Stored on the tenant document as `facebook.page_access_token` / `line.channel_access_token`. They are redacted in JSON responses (the `GET /api/v1/channels` handler scrubs them before sending). For production you should encrypt these at rest with a KMS-backed key.

## Configuration

| Variable                  | Default                                    | Notes                                                       |
| ------------------------- | ------------------------------------------ | ----------------------------------------------------------- |
| `BACKEND_PORT`            | `8080`                                     |                                                             |
| `MONGO_URI`               | `mongodb://localhost:27017`                |                                                             |
| `MONGO_DB`                | `topdee`                                   |                                                             |
| `JWT_SECRET`              | —                                          | Required — service refuses to boot without it.              |
| `JWT_TTL_HOURS`           | `24`                                       |                                                             |
| `AI_SERVICE_URL`          | `http://localhost:8000`                    |                                                             |
| `PLATFORM_SYSTEM_PROMPT`  | (default support-agent prompt)             | The single prompt used for every tenant.                    |
| `PLATFORM_MODEL`          | `gemini-2.0-flash`                         | Forwarded to AI service per request.                        |
| `PLATFORM_TEMPERATURE`    | `0.3`                                      |                                                             |
| `FB_APP_SECRET`           | —                                          | Required for production — used to verify `X-Hub-Signature-256`. |
| `FB_VERIFY_TOKEN`         | `topdee-verify-change-me`                  | Echoed back during webhook subscription verification.       |

## What's stubbed

- LINE webhook: connection works, but inbound messages are not yet processed. Add LINE channel-secret HMAC verification and the Reply API call.
- Embedding runs inline on the upload request — fine for small files, slow for big PDFs. Move to a Redis-backed worker.
- No rate limiting per tenant — add Fiber's `limiter` middleware keyed on `tenant_id`.
- No audit log of human takeovers (and no inbox UI yet either).
- Channel access tokens stored as plaintext in Mongo. Wrap with a KMS / libsodium secretbox before going public.
