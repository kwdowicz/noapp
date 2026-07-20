package app

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed web/*
var webFiles embed.FS

type Server struct {
	db *pgxpool.Pool
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

func New(db *pgxpool.Pool) http.Handler {
	s := &Server{db: db}
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
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database is unavailable")
		return
	}
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
	writeJSON(w, http.StatusOK, task)
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
