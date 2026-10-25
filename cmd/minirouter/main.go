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

	"example.com/minirouter/internal/api"
	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/catalog"
	"example.com/minirouter/internal/router"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	fail := func(msg string, args ...any) {
		log.Error(msg, args...)
		os.Exit(1)
	}
	key := os.Getenv("MINIROUTER_API_KEY")
	if len(key) < 12 {
		fail("set MINIROUTER_API_KEY to at least 12 characters")
	}
	// A catalog file describes models, deployments and aliases; without one,
	// the quick-start catalog comes from provider keys in the environment.
	var cfg catalog.Config
	var err error
	if path := os.Getenv("MINIROUTER_CONFIG"); path != "" {
		if cfg, err = catalog.Load(path); err == nil {
			err = cfg.Validate(os.Getenv)
		}
	} else {
		cfg, err = catalog.FromEnv(os.Getenv)
	}
	if err != nil {
		fail("invalid catalog", "error", err)
	}
	snap, err := catalog.NewSnapshot(cfg)
	if err != nil {
		fail("invalid catalog", "error", err)
	}
	providers, err := cfg.Build(os.Getenv)
	if err != nil {
		fail("invalid catalog", "error", err)
	}
	// Off unless set: reasoning models can legitimately think for minutes
	// before their first token.
	firstEvent, err := time.ParseDuration(env("MINIROUTER_FIRST_EVENT_TIMEOUT", "0s"))
	if err != nil || firstEvent < 0 {
		fail("MINIROUTER_FIRST_EVENT_TIMEOUT must be a duration such as 20s")
	}
	rt := &router.Router{Catalog: catalog.NewRegistry(snap), Providers: providers, FirstEventTimeout: firstEvent}
	app := &api.Server{Router: rt, Store: billing.NewStore(), Key: key, Log: log}
	addr := env("MINIROUTER_ADDR", "127.0.0.1:8080")
	server := &http.Server{Addr: addr, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(c)
	}()
	log.Info("minirouter starting", "address", addr, "models", len(cfg.Models), "deployments", len(cfg.Deployments), "aliases", len(cfg.Aliases))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail("server exited", "error", err)
	}
}
func env(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	return fallback
}
