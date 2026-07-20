# NoApp Project Board

NoApp is a deliberately small project-tracking application built as a clean baseline for observability experiments. It supports users, projects, tasks, a three-column board, task assignment, and status changes. It intentionally contains **no observability instrumentation yet**.

## Architecture

```text
Browser
   |
   | HTTP :8080 (HTML/CSS/JS and JSON REST API)
   v
Go application (app container)
   |
   | PostgreSQL protocol on the private Docker network
   v
PostgreSQL 17 (db container + persistent named volume)
```

The Go binary uses the standard HTTP server and embeds the static UI into the executable. The API accesses PostgreSQL through `pgx`. Docker Compose creates an explicit bridge network named `noapp-network`; PostgreSQL is not published to the host.

## Directory structure

```text
.
|-- cmd/server/main.go          # Process startup and graceful shutdown
|-- internal/app/server.go      # HTTP routes, validation, and SQL access
|-- internal/app/web/           # Embedded browser UI
|   |-- index.html
|   |-- app.js
|   `-- styles.css
|-- db/init.sql                 # Schema, indexes, and starter data
|-- Dockerfile                  # Multi-stage Go image build
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

Check service health:

```powershell
Invoke-RestMethod http://localhost:8080/api/health
```

Stop the stack while retaining database data:

```powershell
docker compose down
```

To remove the containers **and all NoApp database data**, run `docker compose down -v`.

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

# Rebuild after a source change
docker compose up --build -d

# Run Go checks locally (Go 1.24+)
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

For future observability experiments, the natural instrumentation boundaries are incoming HTTP requests, database queries, process/runtime metrics, structured application logs, and context propagation. None are added in this baseline.
