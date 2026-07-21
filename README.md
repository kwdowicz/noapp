# NoApp Project Board

NoApp is a deliberately small project-tracking application built for observability experiments. It supports users, projects, tasks, a three-column board, task assignment, and status changes. Application logs, HTTP request metrics, distributed traces, and continuous performance profiles form a four-signal observability stack.

## Architecture

```text
Browser
   |---- OIDC authorization code + PKCE ----> Keycloak (:8082) ----> auth-db
   |                                               |
   | HTTP :8080 + bearer token                     | OTLP logs, metrics, traces
   v                                               v
NGINX load balancer (round robin)
   |                         |
   v                         v
Go application          Go application ---- OTLP/HTTP logs, metrics, traces ----> OpenTelemetry Collector
(noapp-1)               (noapp-2)
   |
   | PostgreSQL protocol
   v
PostgreSQL 17 (db container + persistent named volume)

OpenTelemetry Collector ---- native OTLP/HTTP ----> Loki ---- queries ----> Grafana
          |
          |---- Prometheus endpoint ----> Prometheus ---- queries ----> Grafana
          `---- OTLP/gRPC traces ----> Tempo ---- queries ----> Grafana

Traffic Simulator UI (:8081) ---- client credentials ----> Keycloak
          `---- synthetic user activity + bearer token ----> NGINX load balancer (:8080)

Two Go application instances + simulator ---- CPU/memory/runtime profiles ----> Pyroscope ---- queries ----> Grafana
```

Keycloak is the dedicated identity and access-management service. It owns login users, credentials, browser sessions, clients, roles, and token issuance in its own PostgreSQL database. The browser uses the OIDC authorization-code flow with PKCE; the simulator and example CLI use OAuth 2.0 client credentials. NoApp validates Keycloak's signed JWT access tokens locally and enforces realm roles at the API boundary. NGINX distributes authorized requests across two identical NoApp instances with round-robin balancing; both instances share the application PostgreSQL database and are private to the Compose network.

The Go binary uses the standard HTTP server and embeds the static UI into the executable. The API accesses PostgreSQL through `pgx`. Incoming requests create server spans, and each PostgreSQL operation creates a child database span. W3C Trace Context and baggage are accepted from callers. Logs written with the request context carry the active trace and span IDs, which provides trace-to-log correlation. Keycloak exports its logs, metrics, and traces over OTLP as `service.name=noapp-auth`; HTTP access logging plus login and admin events are enabled. The Collector sends traces to Tempo, logs to Loki, and metrics to Prometheus. The two app instances and simulator continuously push runtime profiles to Pyroscope. Grafana starts with all data sources provisioned. Docker Compose creates an explicit bridge network named `noapp-network`; only the application load balancer, simulator UI, Keycloak UI, Prometheus, and Grafana are published to the host.

## Directory structure

```text
.
|-- cmd/server/main.go          # Process startup and graceful shutdown
|-- cmd/simulator/main.go       # Traffic simulator process
|-- internal/app/server.go      # HTTP routes, validation, and SQL access
|-- internal/auth/verifier.go   # JWT/JWKS validation and token claims
|-- internal/telemetry/logs.go  # OpenTelemetry Logs SDK and slog bridge
|-- internal/telemetry/metrics.go # OpenTelemetry Metrics SDK and OTLP exporter
|-- internal/telemetry/traces.go  # OpenTelemetry Traces SDK, sampler, and propagation
|-- internal/telemetry/profiles.go # Pyroscope continuous runtime profiling
|-- internal/simulator/         # Workload engine, target API client, and simulator UI
|-- internal/app/web/           # Embedded browser UI
|   |-- index.html
|   |-- app.js
|   `-- styles.css
|-- db/init.sql                 # Schema, indexes, and starter data
|-- otel/collector.yaml         # OTLP logs, metrics, and debug-only traces pipelines
|-- loki/config.yaml            # Local filesystem log storage
|-- nginx/nginx.conf            # Round-robin upstream and reverse proxy
|-- keycloak/noapp-realm.json   # Seeded OIDC clients, roles, and development users
|-- prometheus/prometheus.yaml  # Collector scrape configuration
|-- tempo/tempo.yaml            # Single-binary Tempo with local trace storage
|-- grafana/dashboards/performance-profiles.json # Performance and flame graphs
|-- grafana/provisioning/       # Automatically provisioned data sources
|-- Dockerfile                  # Multi-stage Go image build
|-- Dockerfile.simulator        # Separate simulator image build
|-- compose.yaml                # Replicated app, load balancer, observability stack, and database
|-- go.mod / go.sum
`-- README.md
```

## Run with Docker Desktop

Requirements: Docker Desktop with Docker Compose. No local Go or PostgreSQL installation is required.

```powershell
cd C:\Users\kwdow\dev\obs\noapp
docker compose up --build -d
docker compose ps
```

Keycloak takes longer than the Go services on its first startup. Wait until `auth`, `noapp-1`, `noapp-2`, and `load-balancer` are healthy in `docker compose ps`.

Open <http://localhost:8080>. The browser redirects to Keycloak for login, then returns with an authorization-code flow protected by PKCE. This address is served by NGINX, which balances each API request between `noapp-1` and `noapp-2`.

## Authentication and authorization

This development realm is imported from `keycloak/noapp-realm.json` the first time the authentication database is created. All credentials below are local-development examples and must be replaced outside this sandbox.

| Purpose | Address / username | Password or secret |
|---|---|---|
| Keycloak administration (master realm) | <http://localhost:8082/admin/> — `admin` | `noapp` |
| NoApp administrator (`noapp-admin`) | `admin` | `noapp` |
| Read-only browser user | `viewer` | `viewer` |
| Read/write browser user | `editor` | `editor` |
| Example machine client | `noapp-cli` | `noapp-cli-dev-secret` |
| Simulator service account | `noapp-simulator` | Set in Compose |

Keycloak administrators manage login users, passwords, sessions, clients, and role assignments. The application database's `users` table is a separate business concept used only for task assignees; creating an assignee does not create a login.

| API request | Required realm role |
|---|---|
| `GET` / `HEAD` | `noapp-viewer` |
| `POST` / `PATCH` | `noapp-editor` |
| `/api/health`, `/api/auth/config`, static UI | Public |

`noapp-editor` includes `noapp-viewer`, and `noapp-admin` includes `noapp-editor`. The development stack deliberately gives the master-realm administrator and NoApp-realm administrator the same credentials, but they are separate Keycloak identities. Tokens must be RS256-signed by the `noapp` realm, have issuer `http://localhost:8082/realms/noapp`, and contain the `noapp-api` audience. The replicas refresh signing keys from Keycloak's internal JWKS endpoint and record allowed, unauthenticated, and forbidden decisions in logs, spans, and the `noapp_auth_decision_count_total` metric.

Request a short-lived machine token and call the API from PowerShell:

```powershell
$token = Invoke-RestMethod -Method Post `
  -Uri http://localhost:8082/realms/noapp/protocol/openid-connect/token `
  -ContentType application/x-www-form-urlencoded `
  -Body @{ grant_type = 'client_credentials'; client_id = 'noapp-cli'; client_secret = 'noapp-cli-dev-secret' }
$authHeaders = @{ Authorization = "Bearer $($token.access_token)" }
Invoke-RestMethod http://localhost:8080/api/projects -Headers $authHeaders
```

Create additional machine clients in the Keycloak Admin Console, enable service accounts, add the `noapp-api` audience mapper, and assign only the required realm role to the service account. Do not reuse the seeded development secrets in a deployed environment.

Grafana is available at <http://localhost:3000> with development credentials `admin` / `noapp`. Open **Explore**, select the already-provisioned **Loki** data source, and query:

```logql
{service_name="noapp"}
```

Loki stores log data in the persistent `noapp-loki-data` volume. Grafana configuration and UI state persist in `noapp-grafana-data`.

Keycloak authentication, access, and administration logs use a separate service label:

```logql
{service_name="noapp-auth"}
```

The provisioned **Tempo** data source is available in Grafana Explore. Select Tempo and use the Search tab, or run this TraceQL query to list NoApp traces:

```traceql
{ resource.service.name = "noapp" }
```

Use `{ resource.service.name = "noapp-auth" }` to inspect login, token, admin-console, and other Keycloak traces.

Opening a span provides a **Logs for this span** link to the corresponding Loki records, a **Request rate** link to related Prometheus metrics, and a **Profiles for this span** link for CPU samples captured during that span. Tempo stores local development trace blocks in the persistent `noapp-tempo-data` volume; the single-binary setup uses Tempo's default 14-day block retention.

The **NoApp / NoApp HTTP Latency** dashboard is provisioned automatically. It includes overall P50/P95/P99 latency, percentile history, P95 and average latency by route, a route selector, and request rate for traffic context. Open it directly at <http://localhost:3000/d/noapp-http-latency/noapp-http-latency>.

The **NoApp / NoApp HTTP Errors** dashboard compares normal responses (HTTP 1xx–3xx) with errors (HTTP 4xx–5xx). It includes current percentages, a normal/client-error/server-error donut, history, per-route error percentage, and request rate by response class. Open it directly at <http://localhost:3000/d/noapp-http-errors/noapp-http-errors>.

The **NoApp / NoApp Performance Profiles** dashboard combines P95/P99 latency and throughput with CPU, allocation, and live-heap flame graphs. Its service selector switches between the application and traffic simulator. Open it directly at <http://localhost:3000/d/noapp-performance-profiles/noapp-performance-profiles>, or use **Drilldown > Profiles** for CPU, memory, goroutine, mutex, and blocking analysis and profile comparisons.

Pyroscope profile data persists in the `noapp-pyroscope-*` volumes. Profiles are uploaded every 10 seconds, so a newly started process needs at least one upload interval before it appears in Grafana.

Prometheus is available at <http://localhost:9090>. Its data persists in `noapp-prometheus-data`. In Grafana, select the provisioned **Prometheus** data source and try these PromQL queries:

```promql
# Request rate by route
sum by (http_route) (rate(noapp_http_server_request_count_total[5m]))

# 95th-percentile request duration in seconds
histogram_quantile(0.95, sum by (le) (rate(noapp_http_server_request_duration_seconds_bucket[5m])))
```

The request metrics carry bounded `http_request_method`, `http_route`, and `http_response_status_code` labels. Raw URLs and request IDs are deliberately excluded from metrics to avoid unbounded label cardinality.

Keycloak metrics are exported through OTLP every five seconds and carry `service_name="noapp-auth"`. Open Grafana Explore with Prometheus and use `{service_name="noapp-auth"}` to discover the available JVM, HTTP, database-pool, login-event, and identity metrics. Keycloak's OpenTelemetry logs and metrics integrations are preview/experimental in this pinned release; traces and the management `/metrics` endpoint are supported features.

## Traffic simulator

Open <http://localhost:8081> to control the separate Workday Simulator. It obtains short-lived tokens with the `noapp-simulator` service account and sends them to the same load balancer as browser traffic. **Start workday** creates a fresh synthetic company of 50 users split across five teams, one initial project per team, and a small task backlog. The steady-state workload is intentionally read-heavy:

| Activity | Approximate share |
|---|---:|
| Display an owned project board | 65% |
| Add a task | 15% |
| Move a task to in progress | 9% |
| Complete a task | 7% |
| Create another project | 4% |

Project creation stops at ten projects per run. The engine retains only the IDs created in its current run, so it never changes tasks in pre-existing projects or projects left by an earlier run. Synthetic records remain in PostgreSQL after stopping; this makes the generated history available for later experiments. Recreating the database volume is the clean reset when desired.

The default rate is 30 actions per minute. The UI can change it live from 0.25× (7.5 actions/minute) to 10× (300 actions/minute), or stop the workday entirely. Initialization actions are shown separately in the live activity feed and may briefly produce a higher burst.

The simulator uses `gofakeit` for people, company, project, and task text. Its control API is:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/status` | Current phase, owned objects, statistics, and activity |
| `POST` | `/api/start` | Start a fresh workday |
| `POST` | `/api/stop` | Stop the current workday |
| `PATCH` | `/api/speed` | Set `{ "multiplier": 0.25..10 }` |

Check service health:

```powershell
Invoke-RestMethod http://localhost:8080/api/health
Invoke-RestMethod http://localhost:8082/realms/noapp/.well-known/openid-configuration
```

View the local application logs and the records received by OpenTelemetry:

```powershell
# Readable application and load-balancer output
docker compose logs -f noapp-1 noapp-2 load-balancer

# Keycloak and its dedicated PostgreSQL database
docker compose logs -f auth auth-db

# Collector debug output; look for LogRecord entries and service.name: noapp
docker compose logs -f otel-collector

# Loki and Grafana troubleshooting output
docker compose logs -f loki grafana

# Prometheus status and scrape target
Invoke-RestMethod http://localhost:9090/-/healthy
Invoke-RestMethod http://localhost:9090/api/v1/targets

# Pyroscope and profiling troubleshooting
docker compose logs -f pyroscope noapp-1 noapp-2 traffic-simulator
curl.exe -u admin:noapp http://localhost:3000/api/datasources/uid/pyroscope/health
```

NGINX access records include the selected `upstream` address and return it in the development-only `X-NoApp-Upstream` response header. Repeated requests should alternate between the two replica addresses. Failed connections are retried once against the peer with a one-second connect timeout. Docker's embedded DNS is re-resolved regularly, so a recreated replica can rejoin without hard-coding its container IP.

Each HTTP request produces a structured completion record with its method, path, status code, duration, and request ID. The response returns that ID in `X-Request-ID`. Create and update operations also emit domain records containing safe entity IDs and status values; names, emails, and request bodies are not logged.

The Go app honors the standard `OTEL_EXPORTER_OTLP_ENDPOINT` environment variable. Compose points it to `http://otel-collector:4318`; signal-specific OTLP/HTTP paths such as `/v1/traces` are added by the SDK. `OTEL_RESOURCE_ATTRIBUTES` gives the replicas distinct `service.instance.id` values (`noapp-1` and `noapp-2`) while retaining `service.name=noapp`. Keycloak uses its `KC_TELEMETRY_*` settings to export OTLP/HTTP logs, metrics, and traces to the same Collector under `service.name=noapp-auth`. `PYROSCOPE_SERVER_ADDRESS` points the Go processes to `http://pyroscope:4040` for continuous profiling.

## Traces

Call an endpoint that accesses the database, then inspect the Collector output:

```powershell
Invoke-RestMethod http://localhost:8080/api/projects -Headers $authHeaders
docker compose logs --since=1m otel-collector | Select-String -Pattern "Traces|Trace ID|Span ID|GET /api/projects|SELECT"
```

One trace contains an HTTP server span named from the matched route (for example, `GET /api/projects`) and child PostgreSQL spans. Server errors mark the HTTP span as failed; 4xx responses remain normal server spans because they are commonly expected client outcomes. The development sampler records every trace. This is useful for experimentation but should normally be replaced with lower-rate or tail-based sampling under production traffic.

Tempo runs here in monolithic mode with local filesystem storage, which is appropriate for this development stack. A production deployment should use object storage and revisit retention, sizing, high availability, authentication, and sampling.

Stop the stack while retaining database data:

```powershell
docker compose down
```

To remove the containers **and all NoApp, Keycloak, Loki, Prometheus, Tempo, Pyroscope, and Grafana data**, run `docker compose down -v`. The `noapp-auth-data` volume contains all managed identities, credentials, clients, roles, and sessions.

## REST API overview

All request and response bodies use JSON. Errors have the form `{ "error": "message" }`.

| Method | Path | Access | Purpose |
|---|---|---|---|
| `GET` | `/api/health` | Public | Check the app and database |
| `GET` | `/api/auth/config` | Public | Browser OIDC bootstrap configuration |
| `GET` | `/api/users` | Viewer | List task assignees |
| `POST` | `/api/users` | Editor | Create a task assignee |
| `GET` | `/api/projects` | Viewer | List projects with task counts |
| `POST` | `/api/projects` | Editor | Create a project |
| `GET` | `/api/projects/{id}/tasks` | Viewer | List a project's tasks |
| `POST` | `/api/projects/{id}/tasks` | Editor | Create a task |
| `PATCH` | `/api/tasks/{id}/status` | Editor | Change task status |

Task status values are `todo`, `in_progress`, and `done`.

Example API flow:

```powershell
$project = Invoke-RestMethod -Method Post -Uri http://localhost:8080/api/projects `
  -Headers $authHeaders `
  -ContentType application/json -Body '{"name":"API demo","description":"Created from PowerShell"}'

$task = Invoke-RestMethod -Method Post -Uri "http://localhost:8080/api/projects/$($project.id)/tasks" `
  -Headers $authHeaders `
  -ContentType application/json -Body '{"title":"Try the board","status":"todo"}'

Invoke-RestMethod -Method Patch -Uri "http://localhost:8080/api/tasks/$($task.id)/status" `
  -Headers $authHeaders `
  -ContentType application/json -Body '{"status":"done"}'
```

## Database notes

- The application PostgreSQL database stores assignees, projects, and tasks with foreign keys and database-level status checks.
- Keycloak uses the separate `auth-db` PostgreSQL service and `noapp-auth-data` volume. Login identities never live in the application database.
- `db/init.sql` runs automatically only when the named volume is first created.
- Data persists in the `noapp-postgres-data` named volume across normal restarts.
- The app connects as the Compose-only `noapp` user through the private network. The example credentials are intentionally development-only.
- To apply schema edits to an existing development database, either run the SQL manually or recreate the volume with `docker compose down -v` followed by `docker compose up --build -d`.

Inspect the database:

```powershell
docker compose exec db psql -U noapp -d noapp
docker compose exec db psql -U noapp -d noapp -c "SELECT id, name FROM projects;"
docker compose exec auth-db psql -U keycloak -d keycloak
```

## Common development commands

```powershell
# Follow application and database output
docker compose logs -f

# Follow only generated workload activity
docker compose logs -f traffic-simulator

# Rebuild after a source change
docker compose up --build -d

# Run Go checks locally (Go 1.25+)
go test ./...
go vet ./...

# Format Go code
gofmt -w .\cmd .\internal

# Restart both application replicas
docker compose restart noapp-1 noapp-2

# Show containers and the dedicated network
docker compose ps
docker network inspect noapp-network
```

OpenTelemetry logs, metrics, HTTP server spans, PostgreSQL child spans, propagation, trace/log correlation, Pyroscope runtime profiles, and trace/profile correlation form the current instrumentation layer. The Go traces and metrics signals are stable; the Logs SDK is currently beta, so its pinned dependencies may require coordinated upgrades.
