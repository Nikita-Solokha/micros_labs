// notification-service: SMS клиентам по событиям системы (провайдер замокан).
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
	"github.com/Nikita-Solokha/micros_labs/services/notification-service/app"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "notification-service")
	port := env("PORT", "8084")

	bus := events.FromEnv(log)
	sms := &app.MockSMS{Log: log, Fail: env("SMS_FAIL", "false") == "true"}

	svc := &app.Service{
		Store:     &app.Store{},
		SMS:       sms,
		Processed: idempotency.NewProcessed(),
		Log:       log,
	}
	bus.Subscribe(events.TopicDispatch, "notification-service", svc.HandleEvent)
	bus.Subscribe(events.TopicOrders, "notification-service", svc.HandleEvent)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           httpx.Middleware(log, app.Routes(svc)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bus.Start(ctx)

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
