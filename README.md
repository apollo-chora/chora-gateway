# chora-gateway

## About

`chora-gateway` is the Go backend-for-frontend (BFF) for the five CHORA Angular surfaces: A+, C+, H+, O+, and R+. It exposes REST and GraphQL endpoints, composes responses from CHORA domain services, and carries authenticated tenant and user context to upstream HTTP calls. The service also initializes OpenTelemetry tracing and can publish asynchronous events through the NATS JetStream event bus.

## Quick start

Prerequisites: Go 1.26.1 or newer and access to the CHORA upstream services used by the gateway.

Copy the example environment file and set the required values:

```bash
cp .env.example .env
```

The gateway requires these environment variables at startup:

```text
CHORA_SESSION_SIGNER
CHORA_SESSION_ISSUER
CHORA_SESSION_AUDIENCE
SVC_TENANCY_URL
SVC_CREATION_URL
SVC_CONSUMPTION_URL
SVC_SHARING_URL
SVC_DELIVERY_URL
SVC_GOVERNANCE_URL
SVC_OBSERVABILITY_URL
SVC_NOTIFICATIONS_URL
```

Each `SVC_*_URL` value must be an absolute `http://` or `https://` URL. `CHORA_SESSION_SIGNER` is the HS256 signing key shared with chora-identity.

Run the server:

```bash
go run ./cmd/server
```

By default it listens on port 8080. Set `PORT` to use another port. For a local configuration, `.env.example` also provides defaults for `CHORA_IDENTITY_URL`, tracing, and NATS.

## Usage

The service exposes public health and service-information endpoints:

- `GET /healthz`, `GET /healthz/`, `GET /health` returns a 200 liveness response.
- `GET /readyz` returns the registered route table.
- `GET /version` returns the service name and version.
- `GET /` returns a service description and the five supported surfaces.

Authentication is provided through two session paths:

`POST /api/v1/auth/session/mint` exchanges username/password credentials with chora-identity and mints a Chora session JWT. The route can be disabled with `MINT_DISABLED=true`.

`POST /api/auth/session` creates the older server-side session flow from a Bearer JWT, and `DELETE /api/auth/session` signs out that session.

The BFF composition endpoints are:

| Endpoint | Method | Purpose |
| --- | --- | --- |
| `/bff/aplus/home` | GET | Composes learning path, recent atoms, and companion data. |
| `/bff/cplus/feed` | GET | Returns the C+ social feed view. |
| `/bff/hplus/tenant` | GET | Returns tenant and entitlement data. |
| `/bff/oplus/governance` | GET | Returns governance and audit-event data. |
| `/bff/rplus/courses` | GET | Returns the R+ course view. |

GraphQL is exposed at `/graphql` and `/graphql/`, with aliases at `/api/v1/graphql` and `/api/v1/graphql/`. The current implementation is a hand-coded dispatcher for learner-facing reads and includes engagement, gamification, companion, circle, and stitched `learner` queries. `GET` returns endpoint metadata; `POST` accepts a JSON body containing `query`, with optional `operationName` and `variables`.

`GET /api/proxy/{service}/{path}` is also registered. In the current handler implementation this route returns a skeleton response describing the requested target and the headers that would be stamped for an upstream call.

The gateway uses the real HTTP upstream adapter when `BFF_HTTPUPSTREAM_ENABLED=true`. Otherwise it selects the fake upstream implementation. The HTTP adapter maps gateway operations to the following downstream routes:

| Gateway operation | Upstream |
| --- | --- |
| Learning path | `SVC_CONSUMPTION_URL/api/learning-paths` |
| Recent atoms | `SVC_CREATION_URL/api/atoms?status=published` |
| Companion | `SVC_CONSUMPTION_URL/companion/me` |
| Social feed | `SVC_SHARING_URL/v1/feed` |
| Tenant | `SVC_TENANCY_URL/tenants/{tenant_id}` |
| Governance | `SVC_GOVERNANCE_URL/governance/{tenant_id}` |
| Audit events | `SVC_OBSERVABILITY_URL/events?tenant_id={tenant_id}` |
| Courses | `SVC_DELIVERY_URL/courses` |

When the HTTP upstream adapter is active, outbound requests can carry the Bearer token, tenant and GCID mesh metadata, W3C `traceparent`, and role information from the validated session context.

Tracing is configured with `OTEL_EXPORTER_OTLP_ENDPOINT`. When it is unset, the observability package falls back to stdout. `CHORA_TRACE_UI_URL` controls optional trace deep-links used by O+ responses.

## Development

The repository is a standalone Go module:

```text
github.com/apollo-chora/chora-gateway
```

Build, vet, and test it with the standard Go commands:

```bash
go build ./...
go vet ./...
go test ./...
```

The main executable is in `cmd/server`. HTTP handlers and middleware are under `internal/adapter/http` and `internal/middleware`. HTTP clients and upstream selection live in `internal/adapter/upstream` and `internal/adapter/clients`. BFF aggregation logic is under `internal/aggregator`, domain types and repositories are under `internal/domain`, and the learner-facing GraphQL gateway is under `internal/graphql`.

`Dockerfile` builds the service from the repository root and produces a non-root distroless runtime image. The GitHub Actions workflow publishes `walfa/chora-gateway` for `linux/amd64` and `linux/arm64` on pushes to `main` and on version tags.

For local environment setup, start from `.env.example`. The process performs a boot-time environment check before binding its HTTP port and exits when required variables are missing or malformed.
