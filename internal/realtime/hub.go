package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"noapp/internal/auth"
	"noapp/internal/events"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	authenticateWithin = 5 * time.Second
	writeWithin        = 5 * time.Second
	pingEvery          = 25 * time.Second
	maximumMessageSize = 64 << 10
)

type Hub struct {
	verifier *auth.Verifier
	mu       sync.RWMutex
	clients  map[*client]struct{}
	metrics  hubMetrics
}

type hubMetrics struct {
	connections metric.Int64UpDownCounter
	auth        metric.Int64Counter
	deliveries  metric.Int64Counter
	dropped     metric.Int64Counter
	events      metric.Int64Counter
	latency     metric.Float64Histogram
}

type client struct {
	connection    *websocket.Conn
	send          chan []byte
	cancel        context.CancelFunc
	principal     auth.Principal
	subscriptions map[int64]struct{}
	expiresAt     atomic.Int64
}

type clientMessage struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token,omitempty"`
	ProjectID   int64  `json:"project_id,omitempty"`
}

func NewHub(verifier *auth.Verifier) (*Hub, error) {
	meter := otel.Meter("noapp/realtime")
	connections, err := meter.Int64UpDownCounter("noapp.realtime.connections.active", metric.WithUnit("{connection}"))
	if err != nil {
		return nil, err
	}
	authenticated, err := meter.Int64Counter("noapp.realtime.authentication", metric.WithUnit("{attempt}"))
	if err != nil {
		return nil, err
	}
	deliveries, err := meter.Int64Counter("noapp.realtime.event.deliveries", metric.WithUnit("{delivery}"))
	if err != nil {
		return nil, err
	}
	dropped, err := meter.Int64Counter("noapp.realtime.clients.dropped", metric.WithUnit("{client}"))
	if err != nil {
		return nil, err
	}
	eventCount, err := meter.Int64Counter("noapp.realtime.events.consumed", metric.WithUnit("{event}"))
	if err != nil {
		return nil, err
	}
	latency, err := meter.Float64Histogram("noapp.realtime.event.end_to_end.duration", metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	return &Hub{
		verifier: verifier,
		clients:  make(map[*client]struct{}),
		metrics: hubMetrics{
			connections: connections, auth: authenticated, deliveries: deliveries,
			dropped: dropped, events: eventCount, latency: latency,
		},
	}, nil
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		slog.WarnContext(r.Context(), "WebSocket upgrade rejected", "error", err)
		return
	}
	connection.SetReadLimit(maximumMessageSize)
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{connection: connection, send: make(chan []byte, 64), cancel: cancel, subscriptions: make(map[int64]struct{})}
	defer func() {
		cancel()
		h.remove(c)
		connection.CloseNow()
	}()
	go c.writeLoop(ctx)

	authCtx, authCancel := context.WithTimeout(ctx, authenticateWithin)
	var first clientMessage
	err = wsjson.Read(authCtx, connection, &first)
	authCancel()
	if err != nil || first.Type != "authenticate" || !h.authenticate(ctx, c, first.AccessToken) {
		_ = connection.Close(websocket.StatusPolicyViolation, "authentication required")
		return
	}
	h.add(ctx, c)
	h.sendJSON(c, map[string]any{
		"type": "ready", "subject": c.principal.Subject,
		"authenticated_until": c.principal.ExpiresAt.UTC(),
	})

	for {
		var message clientMessage
		if err := wsjson.Read(ctx, connection, &message); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure && !errors.Is(err, context.Canceled) {
				slog.DebugContext(ctx, "WebSocket read ended", "error", err, "enduser.id", c.principal.Subject)
			}
			return
		}
		switch message.Type {
		case "authenticate":
			if !h.authenticate(ctx, c, message.AccessToken) {
				_ = connection.Close(websocket.StatusPolicyViolation, "authentication failed")
				return
			}
			h.sendJSON(c, map[string]any{"type": "authenticated", "authenticated_until": c.principal.ExpiresAt.UTC()})
		case "subscribe":
			if message.ProjectID < 1 {
				h.sendJSON(c, map[string]any{"type": "error", "error": "a valid project_id is required"})
				continue
			}
			h.subscribe(c, message.ProjectID)
			h.sendJSON(c, map[string]any{"type": "subscribed", "project_id": message.ProjectID})
		case "unsubscribe":
			h.unsubscribe(c, message.ProjectID)
		default:
			h.sendJSON(c, map[string]any{"type": "error", "error": "unsupported message type"})
		}
	}
}

func (h *Hub) authenticate(ctx context.Context, c *client, token string) bool {
	principal, err := h.verifier.Verify(ctx, "Bearer "+token)
	outcome := "allowed"
	if err != nil || !principal.HasRole("noapp-viewer") {
		outcome = "denied"
		h.metrics.auth.Add(ctx, 1, metric.WithAttributes(attribute.String("auth.outcome", outcome)))
		slog.WarnContext(ctx, "WebSocket authentication denied", "error", err)
		return false
	}
	h.mu.Lock()
	c.principal = principal
	c.expiresAt.Store(principal.ExpiresAt.Unix())
	h.mu.Unlock()
	h.metrics.auth.Add(ctx, 1, metric.WithAttributes(attribute.String("auth.outcome", outcome)))
	slog.InfoContext(ctx, "WebSocket authenticated", "enduser.id", principal.Subject, "enduser.name", principal.Username)
	return true
}

func (h *Hub) Broadcast(ctx context.Context, event events.Envelope, payload []byte) {
	attributes := metric.WithAttributes(attribute.String("event.type", event.EventType))
	h.metrics.events.Add(ctx, 1, attributes)
	h.metrics.latency.Record(ctx, time.Since(event.OccurredAt).Seconds(), attributes)

	h.mu.RLock()
	deliveries := int64(0)
	dropped := make([]*client, 0)
	for c := range h.clients {
		if _, subscribed := c.subscriptions[event.ProjectID]; !subscribed {
			continue
		}
		select {
		case c.send <- payload:
			deliveries++
		default:
			dropped = append(dropped, c)
		}
	}
	h.mu.RUnlock()
	if deliveries > 0 {
		h.metrics.deliveries.Add(ctx, deliveries, attributes)
	}
	for _, c := range dropped {
		h.metrics.dropped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "slow_consumer")))
		slog.WarnContext(ctx, "dropping slow WebSocket client", "enduser.id", c.principal.Subject, "project.id", event.ProjectID)
		c.cancel()
	}
	slog.InfoContext(ctx, "board event broadcast",
		"event.id", event.EventID,
		"event.type", event.EventType,
		"project.id", event.ProjectID,
		"task.id", event.TaskID,
		"task.version", event.TaskVersion,
		"realtime.delivery.count", deliveries,
	)
}

func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) add(ctx context.Context, c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.metrics.connections.Add(ctx, 1)
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	_, existed := h.clients[c]
	delete(h.clients, c)
	h.mu.Unlock()
	if existed {
		h.metrics.connections.Add(context.Background(), -1)
		slog.Info("WebSocket disconnected", "enduser.id", c.principal.Subject)
	}
}

func (h *Hub) subscribe(c *client, projectID int64) {
	h.mu.Lock()
	c.subscriptions = map[int64]struct{}{projectID: {}}
	h.mu.Unlock()
	slog.Info("WebSocket project subscribed", "enduser.id", c.principal.Subject, "project.id", projectID)
}

func (h *Hub) unsubscribe(c *client, projectID int64) {
	h.mu.Lock()
	delete(c.subscriptions, projectID)
	h.mu.Unlock()
}

func (h *Hub) sendJSON(c *client, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	select {
	case c.send <- payload:
	default:
		c.cancel()
	}
}

func (c *client) writeLoop(ctx context.Context) {
	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-c.send:
			writeCtx, cancel := context.WithTimeout(ctx, writeWithin)
			err := c.connection.Write(writeCtx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		case <-ticker.C:
			if expires := c.expiresAt.Load(); expires > 0 && time.Now().Unix() >= expires {
				_ = c.connection.Close(websocket.StatusPolicyViolation, "access token expired")
				c.cancel()
				return
			}
			pingCtx, cancel := context.WithTimeout(ctx, writeWithin)
			err := c.connection.Ping(pingCtx)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}
