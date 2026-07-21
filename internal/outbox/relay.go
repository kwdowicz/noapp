package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"noapp/internal/events"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Message struct {
	Topic   string
	Key     []byte
	Value   []byte
	Headers map[string]string
}

type Publisher interface {
	Publish(context.Context, Message) error
}

type Config struct {
	WorkerID    string
	BatchSize   int
	PollEvery   time.Duration
	ClaimFor    time.Duration
	MaxBackoff  time.Duration
	MaxAttempts int
}

type Relay struct {
	db        *pgxpool.Pool
	publisher Publisher
	config    Config
	metrics   relayMetrics
	backlog   atomic.Int64
}

type relayMetrics struct {
	published metric.Int64Counter
	failed    metric.Int64Counter
	dead      metric.Int64Counter
	duration  metric.Float64Histogram
}

type claimedEvent struct {
	sequence     int64
	eventID      string
	projectID    int64
	eventType    string
	payload      []byte
	occurredAt   time.Time
	attemptCount int
}

func New(db *pgxpool.Pool, publisher Publisher, config Config) (*Relay, error) {
	if config.WorkerID == "" {
		return nil, errors.New("outbox worker ID is required")
	}
	if config.BatchSize < 1 {
		config.BatchSize = 100
	}
	if config.PollEvery <= 0 {
		config.PollEvery = 250 * time.Millisecond
	}
	if config.ClaimFor <= 0 {
		config.ClaimFor = 30 * time.Second
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = time.Minute
	}
	if config.MaxAttempts < 1 {
		config.MaxAttempts = 20
	}
	meter := otel.Meter("noapp/outbox-relay")
	published, err := meter.Int64Counter("noapp.outbox.events.published", metric.WithUnit("{event}"))
	if err != nil {
		return nil, err
	}
	failed, err := meter.Int64Counter("noapp.outbox.publish.failures", metric.WithUnit("{failure}"))
	if err != nil {
		return nil, err
	}
	dead, err := meter.Int64Counter("noapp.outbox.events.dead", metric.WithUnit("{event}"))
	if err != nil {
		return nil, err
	}
	duration, err := meter.Float64Histogram("noapp.outbox.publish.duration", metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	r := &Relay{db: db, publisher: publisher, config: config, metrics: relayMetrics{published, failed, dead, duration}}
	_, err = meter.Int64ObservableGauge("noapp.outbox.pending", metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
		observer.Observe(r.backlog.Load())
		return nil
	}), metric.WithUnit("{event}"))
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.config.PollEvery)
	defer ticker.Stop()
	for {
		if err := r.runBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(ctx, "outbox batch failed", "error", err, "outbox.worker.id", r.config.WorkerID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *Relay) Pending(ctx context.Context) (int64, error) {
	var count int64
	err := r.db.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND dead_at IS NULL`).Scan(&count)
	if err == nil {
		r.backlog.Store(count)
	}
	return count, err
}

func (r *Relay) runBatch(ctx context.Context) error {
	batch, err := r.claim(ctx)
	if err != nil {
		return err
	}
	if len(batch) == 0 {
		_, err := r.Pending(ctx)
		return err
	}
	for _, item := range batch {
		if err := r.publishOne(ctx, item); err != nil {
			slog.WarnContext(ctx, "outbox event publish failed",
				"error", err,
				"event.id", item.eventID,
				"event.type", item.eventType,
				"project.id", item.projectID,
				"outbox.sequence", item.sequence,
				"outbox.attempt", item.attemptCount+1,
			)
		}
	}
	_, err = r.Pending(ctx)
	return err
}

func (r *Relay) claim(ctx context.Context) ([]claimedEvent, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin outbox claim: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		WITH candidates AS (
			SELECT sequence
			FROM outbox_events
			WHERE published_at IS NULL
			  AND dead_at IS NULL
			  AND next_attempt_at <= now()
			  AND (claim_until IS NULL OR claim_until < now())
			ORDER BY sequence
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events AS event
		SET claimed_by = $2,
		    claim_until = now() + make_interval(secs => $3)
		FROM candidates
		WHERE event.sequence = candidates.sequence
		RETURNING event.sequence, event.event_id::text, event.project_id,
		          event.event_type, event.payload, event.occurred_at, event.attempt_count`,
		r.config.BatchSize, r.config.WorkerID, int(r.config.ClaimFor.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	defer rows.Close()
	batch := make([]claimedEvent, 0, r.config.BatchSize)
	for rows.Next() {
		var item claimedEvent
		if err := rows.Scan(&item.sequence, &item.eventID, &item.projectID, &item.eventType,
			&item.payload, &item.occurredAt, &item.attemptCount); err != nil {
			return nil, fmt.Errorf("scan claimed outbox event: %w", err)
		}
		batch = append(batch, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read claimed outbox events: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit outbox claim: %w", err)
	}
	return batch, nil
}

func (r *Relay) publishOne(ctx context.Context, item claimedEvent) error {
	event, err := events.Decode(item.payload)
	if err != nil {
		return r.fail(ctx, item, err)
	}
	carrier := propagation.MapCarrier{}
	if event.TraceParent != "" {
		carrier.Set("traceparent", event.TraceParent)
	}
	if event.TraceState != "" {
		carrier.Set("tracestate", event.TraceState)
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	ctx, span := otel.Tracer("noapp/outbox-relay").Start(ctx, "publish "+event.EventType,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination.name", events.Topic),
			attribute.String("event.id", event.EventID),
			attribute.Int64("project.id", event.ProjectID),
		))
	defer span.End()

	headers := map[string]string{
		"event_id":   event.EventID,
		"event_type": event.EventType,
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(headers))
	started := time.Now()
	err = r.publisher.Publish(ctx, Message{
		Topic:   events.Topic,
		Key:     []byte(strconv.FormatInt(event.ProjectID, 10)),
		Value:   item.payload,
		Headers: headers,
	})
	r.metrics.duration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(attribute.String("event.type", event.EventType)))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Kafka publication failed")
		return r.fail(ctx, item, err)
	}
	result, err := r.db.Exec(ctx, `
		UPDATE outbox_events
		SET published_at = now(), claimed_by = NULL, claim_until = NULL, last_error = NULL
		WHERE sequence = $1 AND claimed_by = $2 AND published_at IS NULL`, item.sequence, r.config.WorkerID)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("mark outbox event published: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("outbox publication claim was lost")
	}
	r.metrics.published.Add(ctx, 1, metric.WithAttributes(attribute.String("event.type", event.EventType)))
	slog.InfoContext(ctx, "outbox event published",
		"event.id", event.EventID,
		"event.type", event.EventType,
		"project.id", event.ProjectID,
		"task.id", event.TaskID,
		"task.version", event.TaskVersion,
		"outbox.sequence", item.sequence,
		"messaging.destination.name", events.Topic,
		"messaging.kafka.message.key", string([]byte(strconv.FormatInt(event.ProjectID, 10))),
	)
	return nil
}

func (r *Relay) fail(ctx context.Context, item claimedEvent, cause error) error {
	attempt := item.attemptCount + 1
	delay := retryDelay(attempt, r.config.MaxBackoff)
	dead := attempt >= r.config.MaxAttempts
	_, updateErr := r.db.Exec(ctx, `
		UPDATE outbox_events
		SET attempt_count = attempt_count + 1,
		    next_attempt_at = $3,
		    claimed_by = NULL,
		    claim_until = NULL,
		    last_error = left($4, 2000),
		    dead_at = CASE WHEN $5 THEN now() ELSE dead_at END
		WHERE sequence = $1 AND claimed_by = $2 AND published_at IS NULL`,
		item.sequence, r.config.WorkerID, time.Now().Add(delay), cause.Error(), dead)
	r.metrics.failed.Add(ctx, 1, metric.WithAttributes(attribute.String("event.type", item.eventType)))
	if dead {
		r.metrics.dead.Add(ctx, 1, metric.WithAttributes(attribute.String("event.type", item.eventType)))
		slog.ErrorContext(ctx, "outbox event moved to dead-letter state",
			"event.id", item.eventID, "event.type", item.eventType,
			"project.id", item.projectID, "outbox.sequence", item.sequence,
			"outbox.attempt", attempt, "error", cause)
	}
	if updateErr != nil {
		return errors.Join(cause, fmt.Errorf("record outbox failure: %w", updateErr))
	}
	return cause
}

func retryDelay(attempt int, maximum time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exponent := math.Min(float64(attempt-1), 10)
	delay := time.Duration(math.Pow(2, exponent)) * time.Second
	if delay > maximum {
		return maximum
	}
	return delay
}
