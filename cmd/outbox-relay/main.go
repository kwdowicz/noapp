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

	"noapp/internal/outbox"
	"noapp/internal/telemetry"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
)

type kafkaPublisher struct{ client *kgo.Client }

func (p kafkaPublisher) Publish(ctx context.Context, message outbox.Message) error {
	headers := make([]kgo.RecordHeader, 0, len(message.Headers))
	for key, value := range message.Headers {
		headers = append(headers, kgo.RecordHeader{Key: key, Value: []byte(value)})
	}
	return p.client.ProduceSync(ctx, &kgo.Record{
		Topic: message.Topic, Key: message.Key, Value: message.Value, Headers: headers,
	}).FirstErr()
}

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

	poolConfig, err := pgxpool.ParseConfig(env("DATABASE_URL", "postgres://noapp:noapp@localhost:5432/noapp?sslmode=disable"))
	if err != nil {
		slog.Error("parse database configuration", "error", err)
		os.Exit(1)
	}
	poolConfig.ConnConfig.Tracer = otelpgx.NewTracer()
	db, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		slog.Error("create database pool", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	brokers := strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ",")
	kafka, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(env("KAFKA_CLIENT_ID", "noapp-outbox-relay")),
		kgo.RequiredAcks(kgo.AllISRAcks()),
	)
	if err != nil {
		slog.Error("create Kafka client", "error", err)
		os.Exit(1)
	}
	defer kafka.Close()

	workerID, _ := os.Hostname()
	relay, err := outbox.New(db, kafkaPublisher{kafka}, outbox.Config{WorkerID: workerID})
	if err != nil {
		slog.Error("initialize outbox relay", "error", err)
		os.Exit(1)
	}

	server := healthServer(env("RELAY_ADDR", ":8083"), db, kafka, relay)
	go func() {
		slog.Info("outbox relay health server started", "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("outbox relay health server failed", "error", err)
			cancel()
		}
	}()

	slog.Info("outbox relay started", "kafka.brokers", brokers, "outbox.worker.id", workerID)
	if err := relay.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("outbox relay stopped", "error", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
}

func healthServer(addr string, db *pgxpool.Pool, kafka *kgo.Client, relay *outbox.Relay) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		status := http.StatusOK
		body := map[string]any{"status": "ok"}
		if err := db.Ping(ctx); err != nil {
			status, body["status"], body["database"] = http.StatusServiceUnavailable, "degraded", err.Error()
		}
		if err := kafka.Ping(ctx); err != nil {
			status, body["status"], body["kafka"] = http.StatusServiceUnavailable, "degraded", err.Error()
		}
		if pending, err := relay.Pending(ctx); err == nil {
			body["outbox_pending"] = pending
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func initializeTelemetry(ctx context.Context, environment string) (func(), error) {
	logger, shutdownLogger, err := telemetry.NewLoggerFor(ctx, environment, "noapp-outbox-relay")
	if err != nil {
		return nil, fmt.Errorf("initialize logs: %w", err)
	}
	slog.SetDefault(logger)
	meter, err := telemetry.NewMeterProviderFor(ctx, environment, "noapp-outbox-relay")
	if err != nil {
		return nil, fmt.Errorf("initialize metrics: %w", err)
	}
	tracer, err := telemetry.NewTracerProviderFor(ctx, environment, "noapp-outbox-relay")
	if err != nil {
		return nil, fmt.Errorf("initialize traces: %w", err)
	}
	shutdownProfiler, err := telemetry.NewProfiler(env("PYROSCOPE_SERVER_ADDRESS", ""), "noapp-outbox-relay", environment)
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
