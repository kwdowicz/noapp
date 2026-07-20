package app

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

//go:embed web/*
var webFiles embed.FS

type Server struct {
	db *pgxpool.Pool
}

type httpMetrics struct {
	requestCount    metric.Int64Counter
	requestDuration metric.Float64Histogram
}

type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type Project struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	TaskCount   int64     `json:"task_count"`
}

type Task struct {
	ID           int64     `json:"id"`
	ProjectID    int64     `json:"project_id"`
	AssigneeID   *int64    `json:"assignee_id,omitempty"`
	AssigneeName string    `json:"assignee_name,omitempty"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func New(db *pgxpool.Pool) (http.Handler, error) {
	s := &Server{db: db}
	meter := otel.Meter("noapp/http")
	requestCount, err := meter.Int64Counter(
		"noapp.http.server.request.count",
		metric.WithDescription("Number of completed HTTP requests."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create request counter: %w", err)
	}
	requestDuration, err := meter.Float64Histogram(
		"noapp.http.server.request.duration",
		metric.WithDescription("Duration of completed HTTP requests."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5),
	)
	if err != nil {
		return nil, fmt.Errorf("create request duration histogram: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/users", s.listUsers)
	mux.HandleFunc("POST /api/users", s.createUser)
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("GET /api/projects/{id}/tasks", s.listTasks)
	mux.HandleFunc("POST /api/projects/{id}/tasks", s.createTask)
	mux.HandleFunc("PATCH /api/tasks/{id}/status", s.updateTaskStatus)

	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(static)))
	return requestLogger(mux, httpMetrics{requestCount: requestCount, requestDuration: requestDuration}), nil
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database is unavailable")
		return
	}
	slog.InfoContext(r.Context(), "health check completed", "database.status", "ok")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT id, name, email, created_at FROM users ORDER BY name`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load users")
		return
	}
	defer rows.Close()

	users := make([]User, 0)
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Name, &user.Email, &user.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read users")
			return
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not finish loading users")
		return
	}
	slog.InfoContext(r.Context(), "users listed", "users.count", len(users))
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	if input.Name == "" || input.Email == "" || !strings.Contains(input.Email, "@") {
		writeError(w, http.StatusBadRequest, "name and a valid email are required")
		return
	}

	var user User
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO users (name, email) VALUES ($1, $2)
		RETURNING id, name, email, created_at`, input.Name, input.Email).
		Scan(&user.ID, &user.Name, &user.Email, &user.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "users_email_key") {
			writeError(w, http.StatusConflict, "a user with that email already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not create user")
		return
	}
	slog.InfoContext(r.Context(), "user created", "user.id", user.ID)
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `
		SELECT p.id, p.name, p.description, p.created_at, count(t.id)
		FROM projects p LEFT JOIN tasks t ON t.project_id = p.id
		GROUP BY p.id ORDER BY p.created_at DESC`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load projects")
		return
	}
	defer rows.Close()

	projects := make([]Project, 0)
	for rows.Next() {
		var project Project
		if err := rows.Scan(&project.ID, &project.Name, &project.Description, &project.CreatedAt, &project.TaskCount); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read projects")
			return
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not finish loading projects")
		return
	}
	slog.InfoContext(r.Context(), "projects listed", "projects.count", len(projects))
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "project name is required")
		return
	}

	var project Project
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO projects (name, description) VALUES ($1, $2)
		RETURNING id, name, description, created_at`, input.Name, input.Description).
		Scan(&project.ID, &project.Name, &project.Description, &project.CreatedAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create project")
		return
	}
	slog.InfoContext(r.Context(), "project created", "project.id", project.ID)
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT t.id, t.project_id, t.assignee_id, coalesce(u.name, ''), t.title,
		       t.description, t.status, t.created_at, t.updated_at
		FROM tasks t LEFT JOIN users u ON u.id = t.assignee_id
		WHERE t.project_id = $1 ORDER BY t.created_at`, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load tasks")
		return
	}
	defer rows.Close()

	tasks := make([]Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read tasks")
			return
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not finish loading tasks")
		return
	}
	slog.InfoContext(r.Context(), "project tasks listed", "project.id", projectID, "tasks.count", len(tasks))
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(w, r)
	if !ok {
		return
	}
	var input struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		AssigneeID  *int64 `json:"assignee_id"`
		Status      string `json:"status"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)
	if input.Status == "" {
		input.Status = "todo"
	}
	if input.Title == "" || !validStatus(input.Status) {
		writeError(w, http.StatusBadRequest, "title is required and status must be todo, in_progress, or done")
		return
	}

	row := s.db.QueryRow(r.Context(), `
		WITH inserted AS (
			INSERT INTO tasks (project_id, assignee_id, title, description, status)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING *
		)
		SELECT i.id, i.project_id, i.assignee_id, coalesce(u.name, ''), i.title,
		       i.description, i.status, i.created_at, i.updated_at
		FROM inserted i LEFT JOIN users u ON u.id = i.assignee_id`,
		projectID, input.AssigneeID, input.Title, input.Description, input.Status)
	task, err := scanTask(row)
	if err != nil {
		if isReferenceError(err) {
			writeError(w, http.StatusBadRequest, "project or assignee does not exist")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not create task")
		return
	}
	slog.InfoContext(r.Context(), "task created", "task.id", task.ID, "project.id", task.ProjectID, "task.status", task.Status)
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) updateTaskStatus(w http.ResponseWriter, r *http.Request) {
	taskID, ok := pathID(w, r)
	if !ok {
		return
	}
	var input struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if !validStatus(input.Status) {
		writeError(w, http.StatusBadRequest, "status must be todo, in_progress, or done")
		return
	}

	row := s.db.QueryRow(r.Context(), `
		WITH updated AS (
			UPDATE tasks SET status = $1, updated_at = now() WHERE id = $2 RETURNING *
		)
		SELECT u.id, u.project_id, u.assignee_id, coalesce(a.name, ''), u.title,
		       u.description, u.status, u.created_at, u.updated_at
		FROM updated u LEFT JOIN users a ON a.id = u.assignee_id`, input.Status, taskID)
	task, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update task")
		return
	}
	slog.InfoContext(r.Context(), "task status changed", "task.id", task.ID, "project.id", task.ProjectID, "task.status", task.Status)
	writeJSON(w, http.StatusOK, task)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func requestLogger(next http.Handler, metrics httpMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" || len(requestID) > 128 {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		wrapped := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(wrapped, r)
		if wrapped.status == 0 {
			wrapped.status = http.StatusOK
		}

		level := slog.LevelInfo
		if wrapped.status >= 500 {
			level = slog.LevelError
		} else if wrapped.status >= 400 {
			level = slog.LevelWarn
		}
		slog.LogAttrs(r.Context(), level, "HTTP request completed",
			slog.String("http.request.method", r.Method),
			slog.String("url.path", r.URL.Path),
			slog.Int("http.response.status_code", wrapped.status),
			slog.Float64("http.server.request.duration_ms", float64(time.Since(started).Microseconds())/1000),
			slog.String("request.id", requestID),
		)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		} else if _, routeOnly, found := strings.Cut(route, " "); found {
			route = routeOnly
		}
		attributes := metric.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", wrapped.status),
		)
		metrics.requestCount.Add(r.Context(), 1, attributes)
		metrics.requestDuration.Record(r.Context(), time.Since(started).Seconds(), attributes)
	})
}

func newRequestID() string {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(value)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(row scanner) (Task, error) {
	var task Task
	var assigneeID pgtype.Int8
	err := row.Scan(&task.ID, &task.ProjectID, &assigneeID, &task.AssigneeName, &task.Title,
		&task.Description, &task.Status, &task.CreatedAt, &task.UpdatedAt)
	if assigneeID.Valid {
		task.AssigneeID = &assigneeID.Int64
	}
	return task, err
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func validStatus(status string) bool {
	return status == "todo" || status == "in_progress" || status == "done"
}

func isReferenceError(err error) bool {
	return strings.Contains(err.Error(), "foreign key constraint")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
