# chora-gateway

> The backend-for-frontend for the five CHORA Angular surfaces — A+, C+, H+, O+ and
> R+. It is the only application ingress in the stack.

Go. It composes responses from the domain services, carries authenticated tenant and
user context upstream, and holds a session itself. It has **no database and no
migrations**: all state lives in the services behind it.

## What it is

Every browser request to a Chora feature arrives here first. The gateway does four
things:

- **Terminates sessions.** It validates the session JWT on every protected route and
  forwards tenant, caller and role context to the upstream service.
- **Composes BFF views.** One dashboard call fans out to several services and stitches
  the result, so the browser makes one request instead of six.
- **Serves GraphQL.** A hand-coded learner-facing dispatcher at `/graphql`.
- **Owns the route table.** `GET /readyz` returns it — 51 routes on a healthy
  deployment, with each route's surface, path pattern, backend service and auth mode.

In the stack it is published on **`localhost:8093`** (`${CHORA_GATEWAY_PORT:-8093}` →
container `8080`). With the web frontend on `:4200`, those are the only two application
ports a browser ever touches.

### Many features are gated, and most gates fail quietly

This is the single most useful thing to know about deploying the gateway: a route can
be registered and still answer `404`, `503`, or canned data depending on a variable.
Every gate below is real in `compose.yml`, and several are not obvious:

| Gate | If it is not set | Why it matters |
| --- | --- | --- |
| `BFF_HTTPUPSTREAM_ENABLED=true` | the gateway uses `FakeUpstream` and every BFF call returns **canned data** | the frontend looks alive and talks to nothing — the stack sets this to `true` explicitly for that reason. It governs the original upstream client only; O+ has its own switch, below |
| `CHORA_GATEWAY_UPSTREAM_FAKE` (absent or not `true`) | O+ requires `CHORA_GOVERNANCE_HTTP_ADDR` **and** `CHORA_OBSERVABILITY_HTTP_ADDR`; missing them is a **boot failure** | setting `BFF_HTTPUPSTREAM_ENABLED=true` does not configure O+ |
| `CHORA_PAYMENTS_GRPC_ADDR` (or `CHORA_PAYMENTS_DISABLED=true`) | **boot fails** | the payments client is fail-closed |
| `STRIPE_PUBLISHABLE_KEY_SECRET_ID` (or `STRIPE_CONFIG_DISABLED=true`) | **boot fails** | there is no Stripe account locally, so the stack disables it |
| `CHORA_GOVERNANCE_GRPC_ADDR` | **boot fails** (`log.Fatalf`) | the O+ hydration loaders require it |
| `CHORA_TENANCY_GRPC_ADDR` | transaction-history and tenancy-admin groups register as **404 stubs** | each group also has its own kill switch — `CHORA_TRANSACTION_HISTORY_DISABLED`, `CHORA_TENANCY_ADMIN_DISABLED` — so setting the address does not force them on |
| `CHORA_REALTIME_TICKET_SIGNER_SECRET` (or `…_SECRET_ID`) | the realtime ticket minter is **disabled** | `_SECRET_ID` resolves through the secrets client; leave both unset and SSE consumers cannot connect |
| `CLOSURE_ORCHESTRATOR_URL`, `AI_KERNEL_ORCHESTRATOR_URL` | those route groups answer **503 DEGRADED** | authoring and closure requests fail at the edge |

Four of those are boot failures and the rest are silent degradations. If a feature
"works" but returns nothing useful, check the quiet ones first — and remember that O+ is
switched separately from everything else, so `BFF_HTTPUPSTREAM_ENABLED=true` alone does
not give you real O+ data.

## Prerequisites

| Route | What you need |
| --- | --- |
| In the stack | A working `chora-stack` deployment. Every upstream it fans out to is a service in the same Compose network. |
| Standalone | Go **1.26.1 or newer**, plus reachable HTTP endpoints for the eight `SVC_*_URL` services. There is no in-memory substitute for a missing upstream — the fake adapter returns canned data, which is not a deployment. |
| Either | A session signer of at least 32 bytes, shared with `chora-identity`. |

## Credentials

This service holds **no model-provider credentials**. Its one secret is the session
signing key, and it must match the identity service exactly.

| Value | Where it comes from | Variable |
| --- | --- | --- |
| Session signing key | **you generate it** — and `chora-identity` must use the same value | `CHORA_SESSION_SIGNER` |
| Session issuer / audience | the stack's defaults (`https://api.chora.local`, `chora-local`) | `CHORA_SESSION_ISSUER`, `CHORA_SESSION_AUDIENCE` |
| Realtime ticket signer | the stack's `.env`; shared with `user-event-stream` | `CHORA_REALTIME_TICKET_SIGNER_SECRET` |
| Upstream addresses | the Compose network; not secrets | the eight `SVC_*_URL` values |

Generate the signer once and use it in both places:

```sh
openssl rand -hex 32
```

It must be **at least 32 bytes**. Below that the gateway refuses to boot with
`chora-session loader: CHORA_SESSION_SIGNER must be ≥32 bytes (got N)` and crash-loops —
which is exactly what a placeholder value produces. Because `chora-identity` validates
the same key, a mismatch shows up as sessions that mint but never validate; a wrong
value here fails silently at request time rather than at boot.

`CHORA_REALTIME_TICKET_SIGNER_SECRET` is a different key for a different job: the
gateway mints short-lived realtime tickets and the SSE tier validates them. Without it
the minter is disabled and live features never connect.

Where the stack's shared values come from, and how to obtain the provider keys the
*upstream* services need, is documented once in
[`chora-stack/README.md`](../chora-stack/README.md#credentials).

## Run it in the stack

`compose.yml` does three things:

1. Builds the image from the sibling checkout at `../chora-gateway`.
2. Starts it once ten upstream services have **started** (`service_started`, not
   `service_healthy`) — identity, tenancy, creation, consumption, sharing, delivery,
   governance, notifications, payments, plus both orchestrators.
3. Publishes `8093:8080` — the only application ingress besides the web frontend.

```sh
cd chora-stack
docker compose up -d gateway
docker compose ps gateway
curl -sf http://localhost:8093/readyz >/dev/null && echo ready
```

Note the dependency condition: `service_started`. Compose does **not** wait for those
ten services to be healthy, so the gateway can come up while an upstream is still
failing. Its own routes then fail per request, which is why the readiness check below
matters more here than elsewhere.

What Compose sets beyond the credentials:

| Variable | Stack value | Effect |
| --- | --- | --- |
| `BFF_HTTPUPSTREAM_ENABLED` | `true` | real HTTP fan-out instead of canned data |
| `CHORA_PAYMENTS_GRPC_ADDR` | `payments:9090` | satisfies the fail-closed payments gate |
| `STRIPE_CONFIG_DISABLED` | `true` | satisfies the Stripe gate with no account |
| `CHORA_GOVERNANCE_GRPC_ADDR`, `CHORA_GOVERNANCE_HTTP_ADDR` | `governance:9090`, `http://governance:8080` | the mandatory O+ hydration loader |
| `CHORA_TENANCY_GRPC_ADDR` | `tenancy:9090` | turns the 404 stubs into real routes |
| `CHORA_OBSERVABILITY_HTTP_ADDR`, `CHORA_A2A_HTTP_ADDR` | observability and a2a-gateway | O+ and A2A route groups |
| `CLOSURE_ORCHESTRATOR_URL`, `AI_KERNEL_ORCHESTRATOR_URL` | both orchestrators | removes the `503 DEGRADED` answers |
| `CHORA_IDENTITY_URL`, `SVC_IDENTITY_URL` | `http://identity:8080` | session mint and `/api/me` |

## Run it standalone

```sh
git clone https://github.com/apollo-chora/chora-gateway.git
cd chora-gateway
cp .env.example .env
go run ./cmd/server
```

**It exits if a required variable is missing or malformed.** The boot-time check runs
before the HTTP port is bound, so a misconfigured gateway fails immediately rather than
serving half a route table. The required set is:

```text
CHORA_SESSION_SIGNER   CHORA_SESSION_ISSUER   CHORA_SESSION_AUDIENCE
SVC_TENANCY_URL        SVC_CREATION_URL       SVC_CONSUMPTION_URL
SVC_SHARING_URL        SVC_DELIVERY_URL       SVC_GOVERNANCE_URL
SVC_OBSERVABILITY_URL  SVC_NOTIFICATIONS_URL
```

Each `SVC_*_URL` must be an absolute `http://` or `https://` URL.

That gate is necessary but **not sufficient**: the payments, Stripe and O+ loaders run
afterwards during handler initialisation and can also terminate the process. Satisfying
only the list above gets you past the gate, not to a running server — see the gates table
in [What it is](#what-it-is). The process does not
read `.env` itself — export it:

```sh
set -a; . ./.env; set +a
```

`PORT` selects the listen port (default `8080`). Build, vet and test:

```sh
go build ./...
go vet ./...
go test ./...
docker build -t chora-gateway .
```

The image is a non-root distroless runtime; CI publishes `walfa/chora-gateway` for
`linux/amd64` and `linux/arm64`.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port |
| `CHORA_SESSION_SIGNER` | **required** | HS256 signing key, shared with `chora-identity`; ≥32 bytes |
| `CHORA_SESSION_ISSUER` | required | session `iss` |
| `CHORA_SESSION_AUDIENCE` | required | session `aud` |
| `CHORA_SESSION_TTL_SECONDS` | `3600` | access-session lifetime |
| `CHORA_REFRESH_TTL_SECONDS` | `604800` | refresh-session lifetime |
| `SVC_TENANCY_URL` | **required** | upstream |
| `SVC_CREATION_URL` | **required** | upstream |
| `SVC_CONSUMPTION_URL` | **required** | upstream |
| `SVC_SHARING_URL` | **required** | upstream |
| `SVC_DELIVERY_URL` | **required** | upstream |
| `SVC_GOVERNANCE_URL` | **required** | upstream |
| `SVC_OBSERVABILITY_URL` | **required** | upstream |
| `SVC_NOTIFICATIONS_URL` | **required** | upstream |
| `SVC_IDENTITY_URL`, `CHORA_IDENTITY_URL` | unset (stack: identity:8080) | session mint and `/api/me` |
| `SVC_PAYMENTS_URL` | unset | payments HTTP |
| `BFF_HTTPUPSTREAM_ENABLED` | off → fake adapter | **set this to `true`** for anything real |
| `CHORA_PAYMENTS_GRPC_ADDR` | unset | required unless `CHORA_PAYMENTS_DISABLED=true` |
| `STRIPE_CONFIG_DISABLED` | unset | required unless a Stripe key is configured |
| `CHORA_GOVERNANCE_GRPC_ADDR` | unset | mandatory; boot fails without it |
| `CHORA_TENANCY_GRPC_ADDR` | unset | enables tenancy-admin and transaction-history groups |
| `CHORA_REALTIME_TICKET_SIGNER_SECRET` / `CHORA_REALTIME_TICKET_SIGNER_SECRET_ID` | unset | shared with `user-event-stream`; both unset disables the minter |
| `CHORA_GATEWAY_UPSTREAM_FAKE` | off | O+ fake mode; when not `true`, the O+ loaders require the two HTTP addresses below |
| `CHORA_GOVERNANCE_HTTP_ADDR`, `CHORA_OBSERVABILITY_HTTP_ADDR` | unset | required by the O+ loaders unless O+ is in fake mode |
| `CHORA_TENANCY_GRPC_ADDR` | unset | enables the tenancy-admin and transaction-history groups |
| `CHORA_TRANSACTION_HISTORY_DISABLED`, `CHORA_TENANCY_ADMIN_DISABLED` | off | per-group kill switches; either one 404s its group regardless of the address |
| `CLOSURE_ORCHESTRATOR_URL`, `AI_KERNEL_ORCHESTRATOR_URL` | unset | unset leaves the closure and weakness-resume groups at `503 DEGRADED`; O+ also consumes the kernel URL for its catalogue |
| `MINT_DISABLED` | off | disable `POST /api/v1/auth/session/mint` |
| `CHORA_TRACE_UI_URL` | unset | trace deep-links in O+ responses |
| `NATS_URL` | unset | async event publishing |
| `CHORA_ENV` | — | environment label (stack: `local`) |
| `CHORA_SOURCE_PROJECT` | — | event envelope source project |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | stdout | OTLP/gRPC tracing; unset streams to stdout |

## Verify it works

### It is up

```sh
curl -sf http://localhost:8093/healthz
curl -sf http://localhost:8093/version
curl -sf http://localhost:8093/
```

```json
{"build_date":"...","service":"chora-gateway","version":"0.1.0"}
{"description":"BFF (Backend-For-Frontend) for the 5 CHORA Angular surfaces.","service":"chora-gateway","surfaces":["aplus","cplus","hplus","oplus","rplus"],"...":"..."}
```

`/healthz` (also `/healthz/` and `/health`) is liveness only.

### Its route table is loaded

```sh
curl -s http://localhost:8093/readyz \
  | python -c "import sys,json; d=json.load(sys.stdin); print(d['status'], d['route_count'])"
```

```text
ready 51
```

`readyz` is the most informative readiness probe in the stack: it returns the whole
route table with `surface`, `path_pattern`, `backend_service` and `auth_mode` per route.
A route missing from it is a route that will 404 — which makes this the first place to
look when a feature is unreachable.

### It is really talking to upstreams

This is the check that catches the fake adapter:

```sh
curl -s -H "Authorization: Bearer <session>" http://localhost:8093/bff/aplus/home | head -c 200
```

Real composed data — or a real upstream error — is the pass. Uniform, plausible-looking
canned payloads that never change between calls mean `BFF_HTTPUPSTREAM_ENABLED` is not
`true`. Confirm in the logs, which record the upstream wiring at boot:

```sh
docker compose logs gateway | grep -i upstream
```

### Authentication works end to end

```sh
curl -s -X POST http://localhost:8093/api/v1/auth/session/mint \
  -H 'Content-Type: application/json' \
  -d '{"username":"instructor","password":"instructor"}'
```

A session JWT is the pass. A `401` means the credentials did not match what
`identity-seed` created; a `503` means the mint route is disabled or identity is
unreachable.

## Troubleshooting

- **The frontend works but every view is identical or stale** — `BFF_HTTPUPSTREAM_ENABLED`
  is not `true`, so `FakeUpstream` is serving canned responses. This is the single most
  confusing gateway failure: nothing errors, and the data looks plausible.
- **The service exits at boot naming a variable** — the boot-time check runs before the
  port binds. The most common causes are a short `CHORA_SESSION_SIGNER`, a `SVC_*_URL`
  that is not an absolute `http(s)` URL, or a missing payments/Stripe/governance gate.
- **Sessions mint but every protected route returns `401`** — the signer does not match
  `chora-identity`'s. Both sides must load the same `CHORA_SESSION_SIGNER`.
- **`chora-session loader: CHORA_SESSION_SIGNER must be ≥32 bytes (got N)`** — the key
  is too short, usually a placeholder left in place. Generate one with
  `openssl rand -hex 32`.
- **Tenancy-admin and transaction-history routes return `404`** — `CHORA_TENANCY_GRPC_ADDR`
  is unset, so those groups register as stubs. The route exists in name only.
- **Authoring or closure routes answer `503 DEGRADED`** — `AI_KERNEL_ORCHESTRATOR_URL` or
  `CLOSURE_ORCHESTRATOR_URL` is unset, or the orchestrator behind it is down.
- **Live/poll features never connect** — `CHORA_REALTIME_TICKET_SIGNER_SECRET` is unset,
  so the ticket minter is disabled. It must match the value `user-event-stream` holds.
- **The gateway is up but a feature 404s** — check `/readyz` first. If the path is not in
  the route table it was never registered, which is a configuration gate rather than a
  routing bug.
- **`/api/proxy/{service}/{path}` returns a description instead of proxying** — that is
  the current handler: it returns a skeleton describing the target and the headers it
  would stamp. It is not a working passthrough.
- **A `503` appears only under load** — the gateway waits on `service_started`, not
  health, so an upstream can be failing while the gateway is ready. Its own error names
  the upstream.
- **Traces never reach Tempo** — every service in this stack logs
  `invalid target address http://otel-collector:4317 ... too many colons in address`,
  so spans are dropped. Observed in the running stack; the fix belongs to the shared
  tracing initialiser.

## API surface

| Group | Path | Purpose |
| --- | --- | --- |
| Health and info | `/healthz`, `/health`, `/version`, `/`, `/readyz` | liveness, build info, route table |
| Session (new) | `POST /api/v1/auth/session/mint` | exchange username/password for a Chora session JWT; disable with `MINT_DISABLED=true` |
| Session (refresh) | `POST /api/v1/auth/session/refresh` | rotates a live refresh token into a new session pair; `404`s when the handler is disabled |
| Session (legacy) | `POST /api/auth/session`, `DELETE /api/auth/session` | server-side session from a Bearer JWT; sign out |
| BFF views | see below | composed per-surface responses |
| GraphQL | `/graphql`, `/graphql/`, `/api/v1/graphql`, `/api/v1/graphql/` | hand-coded learner-facing dispatcher |
| Proxy (skeleton) | `/api/proxy/{service}/{path}` | returns a description, not a passthrough |

BFF composition endpoints:

| Endpoint | Composes |
| --- | --- |
| `/bff/aplus/home` | learning path, recent atoms, companion data |
| `/bff/cplus/feed` | the C+ social feed view |
| `/bff/hplus/tenant` | tenant and entitlement data |
| `/bff/oplus/dashboard` | O+ dashboard |
| `/bff/oplus/governance` | governance and audit-event data |
| `/bff/oplus/dimensions`, `/bff/oplus/agents`, `/bff/oplus/prompts`, `/bff/oplus/costs`, `/bff/oplus/eval` | O+ catalogue, agent, prompt, cost and evaluation views |

Transaction history (registered only when the group is enabled, see the gates table):

| Path | |
| --- | --- |
| `/api/v1/me/transactions` | a learner's own transactions |
| `/api/v1/admin/transactions` | the tenant-wide view |
| `/api/v1/admin/transactions/franchisees` | the franchisee sub-view |

Some paths in those groups answer "not implemented" deliberately rather than being
absent — the export and stream variants are the ones to expect that from.
| `/bff/rplus/courses` | the R+ course view |

GraphQL accepts `POST` with `{"query", "operationName"?, "variables"?}` and covers
engagement, gamification, companion, circle, and stitched `learner` queries; `GET`
returns endpoint metadata.

Upstream mapping used by the HTTP adapter:

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

Outbound requests can carry the Bearer token, tenant and GCID mesh metadata, W3C
`traceparent` and role information taken from the validated session context. The
gateway is the trace root: it generates a `traceparent` when the browser does not send
one.

## Architecture

```text
cmd/server/                 entrypoint, boot-time env check, wiring
internal/adapter/http       handlers and routing
internal/middleware         session validation, mesh metadata, tracing
internal/adapter/upstream   HTTP upstream adapter and the fake one
internal/adapter/clients    clients for dependent services
internal/aggregator         BFF composition logic
internal/domain             domain types and repositories
internal/graphql            learner-facing GraphQL dispatcher
```

The piece that matters operationally is `internal/adapter/upstream`: it holds both the
real HTTP adapter and `FakeUpstream`, selected by `BFF_HTTPUPSTREAM_ENABLED`. That
switch is why a gateway can look healthy and return nothing real — but it is not the
only one. The O+ handler is wired separately by `NewOPlusHandlerFromEnv` and gated by
`CHORA_GATEWAY_UPSTREAM_FAKE`, so the two can disagree: real fan-out for the other four
surfaces while O+ still returns fake-mode envelopes from nil clients.

## Events

The gateway is mostly a synchronous fan-out, but it can publish asynchronous events
through the NATS JetStream bus when `NATS_URL` is set. With it unset there is no
publishing path.

The realtime path is separate from the event bus: `CHORA_REALTIME_TICKET_SIGNER_SECRET`
lets the gateway mint short-lived tickets that `chora-user-event-stream`'s SSE tier
validates. Both sides must hold the same secret, and the minter is disabled entirely
when it is absent.

## Migrations

**None.** The gateway has no database of its own, ships no `migrations/` directory and
is not part of the stack's migrate one-shot set. Every piece of state it serves comes
from an upstream service; its own session state is in the signed JWT and, for the legacy
flow, in the identity service.

If you are auditing a deployment and expect a `gateway-migrate` container, there isn't
one and there should not be.
