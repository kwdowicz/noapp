package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestNewTaskEventAndDecode(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4, 5, 6}, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	task := Task{ID: 9, ProjectID: 4, Title: "Ship realtime", Status: "todo", Version: 2, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	event := NewTaskEvent(ctx, TaskCreated, "subject-1", task)

	if event.EventID == "" || event.EventType != TaskCreated || event.TaskVersion != 2 || event.ActorSubject != "subject-1" {
		t.Fatalf("unexpected event: %#v", event)
	}
	if event.TraceParent == "" {
		t.Fatal("expected W3C trace context in event")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.EventID != event.EventID || decoded.Task.ID != task.ID {
		t.Fatalf("decoded event differs: %#v", decoded)
	}
}

func TestDecodeRejectsInvalidEnvelope(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte(`not-json`),
		[]byte(`{"schema_version":99,"event_id":"event","project_id":1,"task_id":1}`),
		[]byte(`{"schema_version":1,"event_id":"","project_id":1,"task_id":1}`),
	} {
		if _, err := Decode(payload); err == nil {
			t.Fatalf("expected payload to be rejected: %s", payload)
		}
	}
}
