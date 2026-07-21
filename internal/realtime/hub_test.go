package realtime

import (
	"context"
	"testing"
	"time"

	"noapp/internal/auth"
	"noapp/internal/events"
)

func TestBroadcastIsScopedToProject(t *testing.T) {
	verifier, err := auth.NewVerifier(auth.Config{Issuer: "issuer", JWKSURL: "https://jwks.test", Audience: "audience"})
	if err != nil {
		t.Fatal(err)
	}
	hub, err := NewHub(verifier)
	if err != nil {
		t.Fatal(err)
	}
	subscribed := &client{send: make(chan []byte, 1), subscriptions: map[int64]struct{}{42: {}}}
	otherProject := &client{send: make(chan []byte, 1), subscriptions: map[int64]struct{}{7: {}}}
	hub.clients[subscribed] = struct{}{}
	hub.clients[otherProject] = struct{}{}
	event := events.Envelope{
		EventID: "event-1", EventType: events.TaskCreated, SchemaVersion: 1,
		ProjectID: 42, TaskID: 9, TaskVersion: 1, OccurredAt: time.Now(),
	}
	payload := []byte(`{"event_id":"event-1"}`)

	hub.Broadcast(context.Background(), event, payload)

	select {
	case got := <-subscribed.send:
		if string(got) != string(payload) {
			t.Fatalf("got %s, want %s", got, payload)
		}
	default:
		t.Fatal("subscribed client did not receive event")
	}
	select {
	case <-otherProject.send:
		t.Fatal("client subscribed to another project received event")
	default:
	}
}
