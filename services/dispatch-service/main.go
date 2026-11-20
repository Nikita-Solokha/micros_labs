// dispatch-service: смены и геопозиции курьеров, подбор ближайшего курьера,
// таймаут поиска (запуск компенсаций саги).
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
	"github.com/Nikita-Solokha/micros_labs/services/dispatch-service/app"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(log *slog.Logger, key, def string) time.Duration {
	d, err := time.ParseDuration(env(key, def))
	if err != nil {
		log.Error("bad duration, using default", "key", key, "default", def)
		d, _ = time.ParseDuration(def)
	}
	return d
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "dispatch-service")
	port := env("PORT", "8082")

	bus := events.FromEnv(log)

	cfg := app.Config{
		SearchTimeout:  envDuration(log, "SEARCH_TIMEOUT", "15m"), // для демонстрации можно 30s
		SearchInterval: envDuration(log, "SEARCH_INTERVAL", "2s"),
		LocationTTL:    envDuration(log, "LOCATION_TTL", "60s"),
		RadiusKm:       15,
	}
	log.Info("config", "searchTimeout", cfg.SearchTimeout.String(), "locationTTL", cfg.LocationTTL.String())

	svc := &app.Service{
		Store:     app.NewStore(),
		Orders:    app.NewHTTPOrderClient(env("ORDER_URL", "http://localhost:8081")),
		Geo:       app.MockGeocoder{},
		Bus:       bus,
		Processed: idempotency.NewProcessed(),
		Cfg:       cfg,
		Log:       log,
	}

	bus.Subscribe(events.TopicOrders, "dispatch-service", svc.HandleEvent)
	bus.Subscribe(events.TopicPayments, "dispatch-service", svc.HandleEvent)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           httpx.Middleware(log, app.Routes(svc)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bus.Start(ctx)
	go svc.Run(ctx) // фоновый поиск курьеров и контроль таймаута

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
