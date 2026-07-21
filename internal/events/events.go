package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

const (
	Topic                = "noapp.board-events.v1"
	TaskCreated          = "task.created"
	TaskStatusChanged    = "task.status_changed"
	CurrentSchemaVersion = 1
)

// Task is the API and event representation of a board item.
type Task struct {
	ID           int64     `json:"id"`
	ProjectID    int64     `json:"project_id"`
	AssigneeID   *int64    `json:"assignee_id,omitempty"`
	AssigneeName string    `json:"assignee_name,omitempty"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	Version      int64     `json:"version"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Envelope is the stable, versioned contract published to Kafka and WebSocket clients.
type Envelope struct {
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	SchemaVersion int       `json:"schema_version"`
	ProjectID     int64     `json:"project_id"`
	TaskID        int64     `json:"task_id"`
	TaskVersion   int64     `json:"task_version"`
	OccurredAt    time.Time `json:"occurred_at"`
	ActorSubject  string    `json:"actor_subject,omitempty"`
	TraceParent   string    `json:"traceparent,omitempty"`
	TraceState    string    `json:"tracestate,omitempty"`
	Task          Task      `json:"task"`
}

func NewTaskEvent(ctx context.Context, eventType, actorSubject string, task Task) Envelope {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return Envelope{
		EventID:       uuid.NewString(),
		EventType:     eventType,
		SchemaVersion: CurrentSchemaVersion,
		ProjectID:     task.ProjectID,
		TaskID:        task.ID,
		TaskVersion:   task.Version,
		OccurredAt:    time.Now().UTC(),
		ActorSubject:  actorSubject,
		TraceParent:   carrier.Get("traceparent"),
		TraceState:    carrier.Get("tracestate"),
		Task:          task,
	}
}

func InsertOutbox(ctx context.Context, tx pgx.Tx, event Envelope) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode outbox event: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (
			event_id, project_id, aggregate_id, aggregate_version,
			event_type, payload, occurred_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		event.EventID, event.ProjectID, event.TaskID, event.TaskVersion,
		event.EventType, payload, event.OccurredAt)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

func Decode(payload []byte) (Envelope, error) {
	var event Envelope
	if err := json.Unmarshal(payload, &event); err != nil {
		return Envelope{}, fmt.Errorf("decode board event: %w", err)
	}
	if event.SchemaVersion != CurrentSchemaVersion || event.EventID == "" || event.ProjectID < 1 || event.TaskID < 1 {
		return Envelope{}, fmt.Errorf("invalid board event envelope")
	}
	return event, nil
}
