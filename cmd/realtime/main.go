package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"noapp/internal/auth"
	"noapp/internal/events"
	"noapp/internal/realtime"
	"noapp/internal/telemetry"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	environment := env("APP_ENV", "development")
	shutdownTelemetry, err := initializeTelemetry(ctx, environment)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer shutdownTelemetry()

	verifier, err := auth.NewVerifier(auth.Config{
		Issuer:   env("AUTH_ISSUER_URL", "http://localhost:8082/realms/noapp"),
		JWKSURL:  env("AUTH_JWKS_URL", "http://localhost:8082/realms/noapp/protocol/openid-connect/certs"),
		Audience: env("AUTH_AUDIENCE", "noapp-api"),
	})
	if err != nil {
		slog.Error("initialize token verifier", "error", err)
		os.Exit(1)
	}
	hub, err := realtime.NewHub(verifier)
	if err != nil {
		slog.Error("initialize WebSocket hub", "error", err)
		os.Exit(1)
	}

	brokers := strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ",")
	kafka, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(env("KAFKA_CLIENT_ID", "noapp-realtime")),
		kgo.ConsumerGroup(env("KAFKA_CONSUMER_GROUP", "noapp-realtime-v1")),
		kgo.ConsumeTopics(events.Topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.DisableAutoCommit(),
	)
	if err != nil {
		slog.Error("create Kafka consumer", "error", err)
		os.Exit(1)
	}
	defer kafka.Close()

	server := newServer(env("REALTIME_ADDR", ":8084"), kafka, hub)
	go func() {
		slog.Info("realtime server started", "address", server.Addr, "kafka.brokers", brokers)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("realtime server stopped unexpectedly", "error", err)
			cancel()
		}
	}()

	consume(ctx, kafka, hub)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
}

func consume(ctx context.Context, kafka *kgo.Client, hub *realtime.Hub) {
	for ctx.Err() == nil {
		fetches := kafka.PollFetches(ctx)
		for _, fetchErr := range fetches.Errors() {
			slog.ErrorContext(ctx, "Kafka fetch failed", "error", fetchErr.Err, "messaging.kafka.partition", fetchErr.Partition)
		}
		records := make([]*kgo.Record, 0)
		fetches.EachRecord(func(record *kgo.Record) {
			records = append(records, record)
			processRecord(ctx, hub, record)
		})
		if len(records) > 0 {
			if err := kafka.CommitRecords(ctx, records...); err != nil {
				slog.ErrorContext(ctx, "Kafka offset commit failed", "error", err, "messaging.batch.message_count", len(records))
			}
		}
	}
}

func processRecord(ctx context.Context, hub *realtime.Hub, record *kgo.Record) {
	carrier := propagation.MapCarrier{}
	for _, header := range record.Headers {
		carrier.Set(header.Key, string(header.Value))
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	ctx, span := otel.Tracer("noapp/realtime").Start(ctx, "consume board event",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination.name", record.Topic),
			attribute.Int("messaging.kafka.partition", int(record.Partition)),
			attribute.Int64("messaging.kafka.offset", record.Offset),
		))
	defer span.End()
	event, err := events.Decode(record.Value)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid event")
		slog.ErrorContext(ctx, "invalid board event skipped", "error", err, "messaging.kafka.partition", record.Partition, "messaging.kafka.offset", record.Offset)
		return
	}
	span.SetAttributes(attribute.String("event.id", event.EventID), attribute.String("event.type", event.EventType), attribute.Int64("project.id", event.ProjectID))
	hub.Broadcast(ctx, event, record.Value)
}

func newServer(addr string, kafka *kgo.Client, hub *realtime.Hub) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /api/realtime", hub)
	mux.HandleFunc("GET /api/realtime/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		status := http.StatusOK
		body := map[string]any{"status": "ok", "connections": hub.ClientCount()}
		if err := kafka.Ping(ctx); err != nil {
			status, body["status"], body["kafka"] = http.StatusServiceUnavailable, "degraded", err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 75 * time.Second}
}

func initializeTelemetry(ctx context.Context, environment string) (func(), error) {
	logger, shutdownLogger, err := telemetry.NewLoggerFor(ctx, environment, "noapp-realtime")
	if err != nil {
		return nil, fmt.Errorf("initialize logs: %w", err)
	}
	slog.SetDefault(logger)
	meter, err := telemetry.NewMeterProviderFor(ctx, environment, "noapp-realtime")
	if err != nil {
		return nil, fmt.Errorf("initialize metrics: %w", err)
	}
	tracer, err := telemetry.NewTracerProviderFor(ctx, environment, "noapp-realtime")
	if err != nil {
		return nil, fmt.Errorf("initialize traces: %w", err)
	}
	shutdownProfiler, err := telemetry.NewProfiler(env("PYROSCOPE_SERVER_ADDRESS", ""), "noapp-realtime", environment)
	if err != nil {
		return nil, fmt.Errorf("initialize profiler: %w", err)
	}
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownProfiler()
		_ = tracer.Shutdown(shutdownCtx)
		_ = meter.Shutdown(shutdownCtx)
		_ = shutdownLogger(shutdownCtx)
	}, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
