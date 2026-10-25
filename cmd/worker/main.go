// Command worker resolves credit holds left behind when the gateway died
// mid-request: failed requests are released, known costs settled, unknown
// costs kept for review. Several workers may run; rows are claimed with
// FOR UPDATE SKIP LOCKED and every resolution is idempotent.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/minirouter/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	url, pepper := os.Getenv("MINIROUTER_DATABASE_URL"), os.Getenv("MINIROUTER_KEY_PEPPER")
	every, err := time.ParseDuration(env("MINIROUTER_WORKER_INTERVAL", "30s"))
	if url == "" || err != nil || every <= 0 {
		log.Error("set MINIROUTER_DATABASE_URL and MINIROUTER_KEY_PEPPER; MINIROUTER_WORKER_INTERVAL must be a positive duration")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	db, err := store.Open(ctx, url, []byte(pepper))
	if err == nil {
		err = db.Migrate(ctx)
	}
	if err != nil {
		log.Error("database unavailable", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		n, err := db.ExpireHolds(ctx, 500)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("expiring holds failed", "error", err)
		case n > 0:
			log.Info("expired holds resolved", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
