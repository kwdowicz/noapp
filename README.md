# NoApp Project Board

NoApp is a deliberately small project-tracking application built for observability experiments for people taht want to learn it and explore it. It supports users, projects, tasks, a three-column board, task assignment, and status changes. Application logs and HTTP request metrics are instrumented with OpenTelemetry; traces are intentionally not included yet.

## Architecture

```text
Browser
   |
   | HTTP :8080 (HTML/CSS/JS and JSON REST API)
   v
Go application (app container) ---- OTLP/HTTP logs ----> OpenTelemetry Collector
   |
   | PostgreSQL protocol
   v
PostgreSQL 17 (db container + persistent named volume)

OpenTelemetry Collector ---- native OTLP/HTTP ----> Loki ---- queries ----> Grafana
          |
          `---- Prometheus endpoint ----> Prometheus ---- queries ----> Grafana

Traffic Simulator UI (:8081) ---- synthetic user activity ----> Go application (:8080)
```

The Go binary uses the standard HTTP server and embeds the static UI into the executable. The API accesses PostgreSQL through `pgx`. The `slog` bridge sends structured records through the OpenTelemetry Logs SDK and its batch processor to the Collector over OTLP/HTTP. The OpenTelemetry Metrics SDK sends request counters and duration histograms through the same OTLP endpoint every five seconds. The Collector forwards logs to Loki and exposes metrics for Prometheus to scrape. Grafana starts with both Loki and Prometheus provisioned. Docker Compose creates an explicit bridge network named `noapp-network`; PostgreSQL, Loki, and the Collector receivers are not published to the host.

## Directory structure

```text
.
|-- cmd/server/main.go          # Process startup and graceful shutdown
|-- cmd/simulator/main.go       # Traffic simulator process
|-- internal/app/server.go      # HTTP routes, validation, and SQL access
|-- internal/telemetry/logs.go  # OpenTelemetry Logs SDK and slog bridge
|-- internal/simulator/         # Workload engine, target API client, and simulator UI
|-- internal/app/web/           # Embedded browser UI
|   |-- index.html
|   |-- app.js
|   `-- styles.css
|-- db/init.sql                 # Schema, indexes, and starter data
|-- otel/collector.yaml         # OTLP logs pipeline and debug exporter
|-- loki/config.yaml            # Local filesystem log storage
|-- prometheus/prometheus.yaml  # Collector scrape configuration
|-- grafana/provisioning/       # Automatically provisioned data sources
|-- Dockerfile                  # Multi-stage Go image build
|-- Dockerfile.simulator        # Separate simulator image build
|-- compose.yaml                # App, database, network, and volume
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

Open <http://localhost:8080>. The initial database contains two sample users, one project, and one task. Use **New project** and **Add task** in the UI; change a task's status with the selector on its card.

Grafana is available at <http://localhost:3000> with development credentials `admin` / `noapp`. Open **Explore**, select the already-provisioned **Loki** data source, and query:

```logql
{service_name="noapp"}
```

Loki stores log data in the persistent `noapp-loki-data` volume. Grafana configuration and UI state persist in `noapp-grafana-data`.

The **NoApp / NoApp HTTP Latency** dashboard is provisioned automatically. It includes overall P50/P95/P99 latency, percentile history, P95 and average latency by route, a route selector, and request rate for traffic context. Open it directly at <http://localhost:3000/d/noapp-http-latency/noapp-http-latency>.

The **NoApp / NoApp HTTP Errors** dashboard compares normal responses (HTTP 1xx–3xx) with errors (HTTP 4xx–5xx). It includes current percentages, a normal/client-error/server-error donut, history, per-route error percentage, and request rate by response class. Open it directly at <http://localhost:3000/d/noapp-http-errors/noapp-http-errors>.

Prometheus is available at <http://localhost:9090>. Its data persists in `noapp-prometheus-data`. In Grafana, select the provisioned **Prometheus** data source and try these PromQL queries:

```promql
# Request rate by route
sum by (http_route) (rate(noapp_http_server_request_count_total[5m]))

# 95th-percentile request duration in seconds
histogram_quantile(0.95, sum by (le) (rate(noapp_http_server_request_duration_seconds_bucket[5m])))
```

The request metrics carry bounded `http_request_method`, `http_route`, and `http_response_status_code` labels. Raw URLs and request IDs are deliberately excluded from metrics to avoid unbounded label cardinality.

## Traffic simulator

Open <http://localhost:8081> to control the separate Workday Simulator. **Start workday** creates a fresh synthetic company of 50 users split across five teams, one initial project per team, and a small task backlog. The steady-state workload is intentionally read-heavy:

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
```

View the local application logs and the records received by OpenTelemetry:

```powershell
# Readable application output
docker compose logs -f app

# Collector debug output; look for LogRecord entries and service.name: noapp
docker compose logs -f otel-collector

# Loki and Grafana troubleshooting output
docker compose logs -f loki grafana

# Prometheus status and scrape target
Invoke-RestMethod http://localhost:9090/-/healthy
Invoke-RestMethod http://localhost:9090/api/v1/targets
```

Each HTTP request produces a structured completion record with its method, path, status code, duration, and request ID. The response returns that ID in `X-Request-ID`. Create and update operations also emit domain records containing safe entity IDs and status values; names, emails, and request bodies are not logged.

The app honors the standard `OTEL_EXPORTER_OTLP_ENDPOINT` environment variable. Compose points it to `http://otel-collector:4318`; the Collector then fans records out to its debug exporter and Loki. Resource records include `service.name=noapp`, `service.version=1.0.0`, and the environment from `APP_ENV`.

Stop the stack while retaining database data:

```powershell
docker compose down
```

To remove the containers **and all NoApp database, Loki, Prometheus, and Grafana data**, run `docker compose down -v`.

## REST API overview

All request and response bodies use JSON. Errors have the form `{ "error": "message" }`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/health` | Check the app and database |
| `GET` | `/api/users` | List users |
| `POST` | `/api/users` | Create a user |
| `GET` | `/api/projects` | List projects with task counts |
| `POST` | `/api/projects` | Create a project |
| `GET` | `/api/projects/{id}/tasks` | List a project's tasks |
| `POST` | `/api/projects/{id}/tasks` | Create a task |
| `PATCH` | `/api/tasks/{id}/status` | Change task status |

Task status values are `todo`, `in_progress`, and `done`.

Example API flow:

```powershell
$project = Invoke-RestMethod -Method Post -Uri http://localhost:8080/api/projects `
  -ContentType application/json -Body '{"name":"API demo","description":"Created from PowerShell"}'

$task = Invoke-RestMethod -Method Post -Uri "http://localhost:8080/api/projects/$($project.id)/tasks" `
  -ContentType application/json -Body '{"title":"Try the board","status":"todo"}'

Invoke-RestMethod -Method Patch -Uri "http://localhost:8080/api/tasks/$($task.id)/status" `
  -ContentType application/json -Body '{"status":"done"}'
```

## Database notes

- PostgreSQL stores users, projects, and tasks with foreign keys and database-level status checks.
- `db/init.sql` runs automatically only when the named volume is first created.
- Data persists in the `noapp-postgres-data` named volume across normal restarts.
- The app connects as the Compose-only `noapp` user through the private network. The example credentials are intentionally development-only.
- To apply schema edits to an existing development database, either run the SQL manually or recreate the volume with `docker compose down -v` followed by `docker compose up --build -d`.

Inspect the database:

```powershell
docker compose exec db psql -U noapp -d noapp
docker compose exec db psql -U noapp -d noapp -c "SELECT id, name FROM projects;"
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

# Restart only the app
docker compose restart app

# Show containers and the dedicated network
docker compose ps
docker network inspect noapp-network
```

OpenTelemetry logging and HTTP metrics form the current instrumentation layer. The Go metrics signal is stable; the Logs SDK is currently beta, so its pinned dependencies may require coordinated upgrades. Future experiments can add traces, database spans, business metrics, and cross-signal correlation without replacing the existing pipelines.
