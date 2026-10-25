package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"example.com/minirouter/internal/api"
	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
	"example.com/minirouter/internal/router"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	key := os.Getenv("MINIROUTER_API_KEY")
	if len(key) < 12 {
		log.Error("set MINIROUTER_API_KEY to at least 12 characters")
		os.Exit(1)
	}
	providers := map[string]provider.Provider{"demo": provider.Demo{}}
	models := []core.Model{{ID: "demo/echo", Provider: "demo", Upstream: "echo", Price: &core.Price{}}}
	// Each configured provider exposes one model. Add more via the model registry.
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		providers["openai"] = &provider.OpenAI{Key: v, Client: provider.NewHTTPClient()}
		models = append(models, core.Model{ID: "openai/" + env("OPENAI_MODEL", "gpt-4.1-mini"), Upstream: env("OPENAI_MODEL", "gpt-4.1-mini"), Provider: "openai", Price: price("OPENAI")})
	}
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		providers["anthropic"] = &provider.Anthropic{Key: v, Client: provider.NewHTTPClient()}
		models = append(models, core.Model{ID: "anthropic/" + env("ANTHROPIC_MODEL", "claude-haiku-4-5"), Upstream: env("ANTHROPIC_MODEL", "claude-haiku-4-5"), Provider: "anthropic", Price: price("ANTHROPIC")})
	}
	if v := os.Getenv("GEMINI_API_KEY"); v != "" {
		providers["google"] = &provider.Gemini{Key: v, Client: provider.NewHTTPClient()}
		models = append(models, core.Model{ID: "google/" + env("GEMINI_MODEL", "gemini-2.5-flash"), Upstream: env("GEMINI_MODEL", "gemini-2.5-flash"), Provider: "google", Price: price("GEMINI")})
	}
	app := &api.Server{Router: &router.Router{Models: models, Providers: providers}, Store: billing.NewStore(), Key: key, Log: log}
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
	log.Info("minirouter starting", "address", addr, "available_models", len(models)+1, "demo_enabled", true)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server exited", "error", err)
		os.Exit(1)
	}
}
func env(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	return fallback
}
func price(prefix string) *core.Price {
	a, hasA := rate(prefix + "_INPUT_USD_PER_1M")
	b, hasB := rate(prefix + "_OUTPUT_USD_PER_1M")
	if !hasA || !hasB {
		return nil
	}
	cache, hasCache := rate(prefix + "_CACHE_READ_USD_PER_1M")
	write, hasWrite := rate(prefix + "_CACHE_WRITE_USD_PER_1M")
	if !hasCache {
		cache = a
	}
	if !hasWrite {
		write = a
	}
	return &core.Price{InputMicrosPerMillion: a, OutputMicrosPerMillion: b, CacheReadMicrosPerMillion: cache, CacheWriteMicrosPerMillion: write}
}

// parse fixed-point USD/1M exactly into microUSD, without float rounding.
func rate(key string) (int64, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 6 {
		panic("invalid rate: " + key)
	}
	for _, c := range raw {
		if !(c >= '0' && c <= '9') && c != '.' {
			panic("invalid rate: " + key)
		}
	}
	whole := int64(0)
	for _, c := range parts[0] {
		whole = whole*10 + int64(c-'0')
	}
	if whole > 100000 {
		panic("rate is too high: " + key)
	}
	frac := int64(0)
	if len(parts) == 2 {
		if len(parts[1]) == 0 || len(parts[1]) > 6 {
			panic("rate must have at most 6 decimal places: " + key)
		}
		for _, c := range parts[1] {
			frac = frac*10 + int64(c-'0')
		}
		for i := len(parts[1]); i < 6; i++ {
			frac *= 10
		}
	}
	return whole*1_000_000 + frac, true
}
