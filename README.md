# chora-gateway

> Production BFF (Backend-For-Frontend) / API gateway for the 5 CHORA Angular
> surfaces (A+, C+, H+, O+, R+). Aggregates upstream domain services into
> per-surface composed views; acts as the trace ROOT.

Standalone, provider-neutral service. Shared Chora modules are consumed as Go modules
(`github.com/apollo-chora/chora-common`, `github.com/apollo-chora/chora-contracts`).
Asynchronous delivery uses NATS JetStream (`chora-common/eventbus`); tracing
uses OTLP (`OTEL_EXPORTER_OTLP_ENDPOINT`).

## Hexagonal layout

```
cmd/server/                                  — main + boot env gate + session + mint + stripe-config loaders
internal/domain/{route,session,aggregate}/   — pure domain (no infra imports)
internal/adapter/{http,inmem,upstream,clients}/ — port implementations
internal/aggregator/{phyllis,social,...}/    — outbound fan-out aggregators
internal/graphql/{resolvers,schema.go}       — federated GraphQL (learner reads)
internal/observability/                      — OTLP + W3C traceparent
```

## Auth

`POST /api/v1/auth/session/mint` exchanges a username/password pair for a
Chora session JWT:

```json
// request
{"username": "alice", "password": "..."}
// 200
{"access_token": "<jwt>", "token_type": "Bearer", "expires_in": 3600,
 "gcid": "<uuid>", "memberships": [{"tenant_id": "<uuid>", "roles": ["learner"]}]}
// 401
{"error": {"code": "INVALID_CREDENTIALS", "message": "..."}}
```

Flow: the gateway calls chora-identity `POST /v1/auth/verify-credentials`
(`CHORA_IDENTITY_URL`, default `http://identity:8080`) and mints an HS256
session JWT from the authoritative `gcid`, `active_tenant_id` and
`active_tenant_roles` it returns. The JWT is signed with `CHORA_SESSION_SIGNER`
(shared with chora-identity; no key exchange) and carries `iss`, `aud`, `sub`,
`gcid`, `tenant_id`, `email`, `roles`, `role_summary`, `iat`, `exp`.

The `/api/*` trust boundary validates that session JWT (`chorasession.Validator`)
and stamps `servicemesh.MeshClaims{GCID, TenantID, Roles}`. The validated
session's tenant stays authoritative; a client-supplied tenant is only a request
checked against it (`tenant_scope.go`).

The `platform_operator` role is membership-backed (it arrives in
`active_tenant_roles`); it is not stamped from any email list. A member holding
`instructor` additionally carries the derived `training_admin` label.

## Endpoints

| Path                                              | Method   | Auth          | Description |
|---------------------------------------------------|----------|---------------|-------------|
| `/healthz`, `/healthz/`, `/health`                | GET      | public        | liveness |
| `/readyz`                                         | GET      | public        | route-table dump |
| `/version`                                        | GET      | public        | service banner |
| `/api/auth/session`                               | POST/DEL | public        | mint/revoke opaque session |
| `/bff/{aplus,cplus,hplus,oplus,rplus}/...`        | GET      | session-based | per-surface composed views |
| `/api/v1/auth/session/mint`                       | POST     | public        | username/password → Chora session JWT |
| `/api/me`, `/api/me/roles`, `/api/tenants/me`     | GET      | ChoraSession  | Phyllis identity fan-out |
| `/api/courses`, `/api/catalog`, `/api/enrollments`| GET/POST | ChoraSession  | Phyllis course/catalog fan-out |
| `/api/atoms/{id}`, `.../feedback`                 | GET/POST | ChoraSession  | Phyllis atom fan-out |
| `/api/ai/generate`                                | POST     | ChoraSession  | Model Broker Gateway proxy |
| `/api/companion/{me,daily-dose}`                   | GET      | ChoraSession  | Phyllis Companion fan-out |
| `/api/me/consents`, `.../grant`                   | GET/POST | ChoraSession  | GDPR consent center |
| `/api/me/data-export`                             | POST     | ChoraSession  | GDPR Art. 15/20 |
| `/api/me/account-closure`                         | POST     | ChoraSession  | GDPR Art. 17 (federated saga trigger) |
| `/v1/feed`, `/v1/me/social`, `/v1/posts/*`, ...   | GET/POST | ChoraSession  | C+ social aggregator |
| `/graphql`, `/graphql/`                           | GET/POST | ChoraSession  | Federated learner-read schema |
| `/api/proxy/{service}/*`                          | any      | session-based | generic upstream proxy |

## Environment configuration

No inline URLs / secrets / audience identifiers in source. All values resolve
from environment variables. See `.env.example`.

### Boot-time fail-loud env gate

`CheckBootEnv` runs in `main()` BEFORE handler registration and exits 1 with a
clear log on any missing required env var.

| Env var | Required | Purpose |
|---|---|---|
| `CHORA_SESSION_SIGNER` | yes | HS256 signing key (≥32 bytes), shared with chora-identity |
| `CHORA_SESSION_ISSUER` | yes | iss claim baked into minted JWTs |
| `CHORA_SESSION_AUDIENCE` | yes | aud claim baked into minted JWTs |
| `CHORA_IDENTITY_URL` | no | chora-identity base URL (default `http://identity:8080`) |
| `CHORA_SESSION_TTL_SECONDS` | no | session lifetime (default 3600) |
| `CHORA_SOURCE_PROJECT` | no | logical project label (default `chora-local`) |
| `SVC_TENANCY_URL` | yes | chora-tenancy base URL |
| `SVC_CREATION_URL` | yes | chora-creation base URL |
| `SVC_CONSUMPTION_URL` | yes | chora-consumption base URL |
| `SVC_SHARING_URL` | yes | chora-sharing base URL |
| `SVC_DELIVERY_URL` | yes | chora-delivery base URL |
| `SVC_GOVERNANCE_URL` | yes | chora-governance base URL |
| `SVC_OBSERVABILITY_URL` | yes | chora-observability base URL |
| `SVC_NOTIFICATIONS_URL` | yes | chora-notifications base URL |

`SVC_*_URL` values must parse as absolute http(s) URLs.

### Tracing

`OTEL_EXPORTER_OTLP_ENDPOINT` (when unset, spans stream to stdout). Trace
deep-links in O+ responses use `CHORA_TRACE_UI_URL` (self-hosted tracing UI;
unset omits the link).

## Build & test

```bash
go build ./...
go vet ./...
go test ./...
```
