// order-service: жизненный цикл заказа, оркестрация первого шага саги.
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
	"github.com/Nikita-Solokha/micros_labs/services/order-service/app"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "order-service")
	port := env("PORT", "8081")

	// Брокер: Kafka, если задан KAFKA_BROKERS, иначе in-memory.
	bus := events.FromEnv(log)

	svc := &app.Service{
		Store:     app.NewStore(),
		Payments:  app.NewHTTPPaymentClient(env("PAYMENT_URL", "http://localhost:8083")),
		Bus:       bus,
		Processed: idempotency.NewProcessed(),
		Log:       log,
	}

	bus.Subscribe(events.TopicPayments, "order-service", svc.HandleEvent)
	bus.Subscribe(events.TopicDispatch, "order-service", svc.HandleEvent)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           httpx.Middleware(log, app.Routes(svc, idempotency.NewStore())),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bus.Start(ctx) // начинаем читать подписки

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
	_ = bus.Close()
}
