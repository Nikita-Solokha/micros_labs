// payment-service: платежи, вебхуки банка, возвраты (компенсация саги).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Nikita-Solokha/micros_labs/pkg/events"
	"github.com/Nikita-Solokha/micros_labs/pkg/httpx"
	"github.com/Nikita-Solokha/micros_labs/pkg/idempotency"
	"github.com/Nikita-Solokha/micros_labs/services/payment-service/app"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "payment-service")

	port := env("PORT", "8083")

	// Брокер: пока in-memory. На шаге с docker-compose подключим Kafka.
	bus := events.NewMemoryBus(log)

	svc := &app.Service{
		Store:         app.NewStore(),
		Gateway:       app.NewHTTPGateway(env("BANK_URL", "http://localhost:9090")),
		Bus:           bus,
		Processed:     idempotency.NewProcessed(),
		WebhookSecret: env("WEBHOOK_SECRET", "dev-secret"),
		CallbackURL:   env("WEBHOOK_URL", "http://localhost:"+port+"/api/v1/payments/webhook"),
		Log:           log,
	}

	// Подписка: курьер не найден → возврат денег.
	bus.Subscribe(events.TopicDispatch, "payment-service", func(ctx context.Context, e events.Envelope) error {
		if e.EventType != events.CourierSearchFail {
			return nil
		}
		return svc.OnSearchFailed(ctx, e)
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           httpx.Middleware(log, app.Routes(svc, idempotency.NewStore())),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shCtx)
}
