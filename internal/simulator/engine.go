package simulator

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/brianvoe/gofakeit/v7"
)

const (
	workforceSize = 50
	teamCount     = 5
	baseRate      = 30.0
	maxProjects   = 10
)

type Engine struct {
	mu       sync.RWMutex
	client   *apiClient
	faker    *gofakeit.Faker
	running  bool
	phase    string
	speed    float64
	cancel   context.CancelFunc
	session  string
	started  time.Time
	teams    []team
	projects []ownedProject
	stats    runStats
	recent   []Event
}

type team struct {
	Name       string
	WorkerIDs  []int64
	ProjectIDs []int64
}

type ownedProject struct {
	ID        int64
	TeamIndex int
	Tasks     map[int64]*ownedTask
}

type ownedTask struct {
	ID     int64
	Status string
}

type runStats struct {
	TotalActions int64            `json:"total_actions"`
	Errors       int64            `json:"errors"`
	Actions      map[string]int64 `json:"actions"`
}

type Event struct {
	At      time.Time `json:"at"`
	Action  string    `json:"action"`
	Detail  string    `json:"detail"`
	Success bool      `json:"success"`
}

type Status struct {
	Running                bool           `json:"running"`
	Phase                  string         `json:"phase"`
	Speed                  float64        `json:"speed"`
	BaseActionsPerMinute   float64        `json:"base_actions_per_minute"`
	EffectiveActionsPerMin float64        `json:"effective_actions_per_minute"`
	Session                string         `json:"session"`
	StartedAt              time.Time      `json:"started_at,omitempty"`
	Teams                  int            `json:"teams"`
	Workers                int            `json:"workers"`
	Projects               int            `json:"projects"`
	Tasks                  map[string]int `json:"tasks"`
	Stats                  runStats       `json:"stats"`
	Recent                 []Event        `json:"recent"`
}

func NewEngine(targetURL string) *Engine {
	return &Engine{
		client: newAPIClient(targetURL),
		faker:  gofakeit.New(0),
		phase:  "stopped",
		speed:  1,
		stats:  runStats{Actions: make(map[string]int64)},
	}
}

func (e *Engine) Start(ctx context.Context) (Status, error) {
	if err := e.client.health(ctx); err != nil {
		return e.Status(), fmt.Errorf("target app is unavailable: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return e.statusLocked(), nil
	}

	runCtx, cancel := context.WithCancel(context.Background())
	e.running = true
	e.phase = "initializing"
	e.cancel = cancel
	e.session = strconv.FormatInt(time.Now().UnixNano(), 36)
	e.started = time.Now().UTC()
	e.teams = nil
	e.projects = nil
	e.stats = runStats{Actions: make(map[string]int64)}
	e.recent = nil
	session := e.session
	go e.run(runCtx, session)
	return e.statusLocked(), nil
}

func (e *Engine) Stop() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running && e.cancel != nil {
		e.phase = "stopping"
		e.cancel()
	}
	return e.statusLocked()
}

func (e *Engine) SetSpeed(speed float64) (Status, error) {
	if speed < 0.25 || speed > 10 {
		return e.Status(), fmt.Errorf("speed must be between 0.25 and 10")
	}
	e.mu.Lock()
	e.speed = speed
	status := e.statusLocked()
	e.mu.Unlock()
	return status, nil
}

func (e *Engine) Status() Status {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.statusLocked()
}

func (e *Engine) statusLocked() Status {
	workers := 0
	tasks := map[string]int{"todo": 0, "in_progress": 0, "done": 0}
	for _, group := range e.teams {
		workers += len(group.WorkerIDs)
	}
	for _, project := range e.projects {
		for _, task := range project.Tasks {
			tasks[task.Status]++
		}
	}
	actions := make(map[string]int64, len(e.stats.Actions))
	for key, value := range e.stats.Actions {
		actions[key] = value
	}
	recent := append([]Event(nil), e.recent...)
	return Status{
		Running:                e.running,
		Phase:                  e.phase,
		Speed:                  e.speed,
		BaseActionsPerMinute:   baseRate,
		EffectiveActionsPerMin: baseRate * e.speed,
		Session:                e.session,
		StartedAt:              e.started,
		Teams:                  len(e.teams),
		Workers:                workers,
		Projects:               len(e.projects),
		Tasks:                  tasks,
		Stats: runStats{
			TotalActions: e.stats.TotalActions,
			Errors:       e.stats.Errors,
			Actions:      actions,
		},
		Recent: recent,
	}
}

func (e *Engine) run(ctx context.Context, session string) {
	defer func() {
		e.mu.Lock()
		if e.session == session {
			e.running = false
			e.phase = "stopped"
			e.cancel = nil
		}
		e.mu.Unlock()
	}()

	if !e.bootstrap(ctx, session) {
		return
	}
	e.mu.Lock()
	if e.session == session {
		e.phase = "running"
	}
	e.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		e.runAction(ctx)
		e.mu.RLock()
		speed := e.speed
		e.mu.RUnlock()
		delay := time.Duration(float64(time.Minute) / (baseRate * speed))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (e *Engine) bootstrap(ctx context.Context, session string) bool {
	teams := make([]team, teamCount)
	for i := range teams {
		teams[i].Name = fmt.Sprintf("%s %s", e.faker.Color(), e.faker.Animal())
	}
	e.mu.Lock()
	e.teams = teams
	e.mu.Unlock()

	for i := 0; i < workforceSize; i++ {
		if ctx.Err() != nil {
			return false
		}
		name := e.faker.Name()
		email := fmt.Sprintf("sim.%s.%02d@example.test", session, i+1)
		user, err := e.client.createUser(ctx, name, email)
		if err != nil {
			e.record("create_user", "Could not create a simulated worker", false, err)
			continue
		}
		teamIndex := i % teamCount
		e.mu.Lock()
		e.teams[teamIndex].WorkerIDs = append(e.teams[teamIndex].WorkerIDs, user.ID)
		e.mu.Unlock()
		e.record("create_user", fmt.Sprintf("Worker %d joined team %d", user.ID, teamIndex+1), true, nil)
	}

	for i := 0; i < teamCount; i++ {
		if ctx.Err() != nil {
			return false
		}
		projectID, ok := e.createProject(ctx, i)
		if !ok {
			continue
		}
		for range 2 {
			e.createTask(ctx, projectID)
		}
	}
	return ctx.Err() == nil
}

func (e *Engine) runAction(ctx context.Context) {
	roll := rand.IntN(100)
	switch {
	case roll < 65:
		e.viewBoard(ctx)
	case roll < 80:
		e.createTask(ctx, 0)
	case roll < 89:
		e.progressTask(ctx, "todo", "in_progress", "start_task")
	case roll < 96:
		e.progressTask(ctx, "in_progress", "done", "complete_task")
	default:
		e.createAnotherProject(ctx)
	}
}

func (e *Engine) viewBoard(ctx context.Context) {
	e.mu.RLock()
	if len(e.projects) == 0 {
		e.mu.RUnlock()
		return
	}
	projectID := e.projects[rand.IntN(len(e.projects))].ID
	e.mu.RUnlock()
	err := e.client.listProjectTasks(ctx, projectID)
	e.record("view_board", fmt.Sprintf("Viewed the board for project %d", projectID), err == nil, err)
}

func (e *Engine) createAnotherProject(ctx context.Context) {
	e.mu.RLock()
	if len(e.projects) >= maxProjects {
		e.mu.RUnlock()
		e.viewBoard(ctx)
		return
	}
	eligible := make([]int, 0, teamCount)
	for i, group := range e.teams {
		if len(group.ProjectIDs) < 2 && len(group.WorkerIDs) > 0 {
			eligible = append(eligible, i)
		}
	}
	e.mu.RUnlock()
	if len(eligible) == 0 {
		e.viewBoard(ctx)
		return
	}
	e.createProject(ctx, eligible[rand.IntN(len(eligible))])
}

func (e *Engine) createProject(ctx context.Context, teamIndex int) (int64, bool) {
	name := fmt.Sprintf("%s %s", e.faker.Company(), e.faker.Noun())
	description := e.faker.Sentence(12)
	project, err := e.client.createProject(ctx, name, description)
	if err != nil {
		e.record("create_project", fmt.Sprintf("Team %d could not create a project", teamIndex+1), false, err)
		return 0, false
	}
	e.mu.Lock()
	e.projects = append(e.projects, ownedProject{ID: project.ID, TeamIndex: teamIndex, Tasks: make(map[int64]*ownedTask)})
	e.teams[teamIndex].ProjectIDs = append(e.teams[teamIndex].ProjectIDs, project.ID)
	e.mu.Unlock()
	e.record("create_project", fmt.Sprintf("Team %d created project %d", teamIndex+1, project.ID), true, nil)
	return project.ID, true
}

func (e *Engine) createTask(ctx context.Context, requestedProjectID int64) {
	e.mu.RLock()
	if len(e.projects) == 0 {
		e.mu.RUnlock()
		return
	}
	project := e.projects[rand.IntN(len(e.projects))]
	if requestedProjectID != 0 {
		for _, candidate := range e.projects {
			if candidate.ID == requestedProjectID {
				project = candidate
				break
			}
		}
	}
	workers := append([]int64(nil), e.teams[project.TeamIndex].WorkerIDs...)
	e.mu.RUnlock()
	if len(workers) == 0 {
		return
	}
	assigneeID := workers[rand.IntN(len(workers))]
	task, err := e.client.createTask(ctx, project.ID, assigneeID, e.faker.Sentence(6), e.faker.Sentence(14))
	if err != nil {
		e.record("create_task", fmt.Sprintf("Could not add a task to project %d", project.ID), false, err)
		return
	}
	e.mu.Lock()
	for i := range e.projects {
		if e.projects[i].ID == project.ID {
			e.projects[i].Tasks[task.ID] = &ownedTask{ID: task.ID, Status: task.Status}
			break
		}
	}
	e.mu.Unlock()
	e.record("create_task", fmt.Sprintf("Added task %d to project %d", task.ID, project.ID), true, nil)
}

func (e *Engine) progressTask(ctx context.Context, from, to, action string) {
	type candidate struct {
		projectID int64
		taskID    int64
	}
	e.mu.RLock()
	candidates := make([]candidate, 0)
	for _, project := range e.projects {
		for _, task := range project.Tasks {
			if task.Status == from {
				candidates = append(candidates, candidate{projectID: project.ID, taskID: task.ID})
			}
		}
	}
	e.mu.RUnlock()
	if len(candidates) == 0 {
		e.createTask(ctx, 0)
		return
	}
	selected := candidates[rand.IntN(len(candidates))]
	_, err := e.client.updateTaskStatus(ctx, selected.taskID, to)
	if err != nil {
		e.record(action, fmt.Sprintf("Could not move task %d to %s", selected.taskID, to), false, err)
		return
	}
	e.mu.Lock()
	for i := range e.projects {
		if task := e.projects[i].Tasks[selected.taskID]; task != nil {
			task.Status = to
			break
		}
	}
	e.mu.Unlock()
	e.record(action, fmt.Sprintf("Moved task %d in project %d to %s", selected.taskID, selected.projectID, to), true, nil)
}

func (e *Engine) record(action, detail string, success bool, err error) {
	event := Event{At: time.Now().UTC(), Action: action, Detail: detail, Success: success}
	e.mu.Lock()
	e.stats.TotalActions++
	e.stats.Actions[action]++
	if !success {
		e.stats.Errors++
	}
	e.recent = append([]Event{event}, e.recent...)
	if len(e.recent) > 30 {
		e.recent = e.recent[:30]
	}
	e.mu.Unlock()
	if err != nil {
		slog.Warn("simulation action failed", "action", action, "detail", detail, "error", err)
	} else {
		slog.Info("simulation action completed", "action", action, "detail", detail)
	}
}
