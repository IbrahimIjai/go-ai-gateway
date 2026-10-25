package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// readyz answers whether this instance can serve traffic: its database (in
// PostgreSQL mode) answers and at least one deployment is not benched or
// behind an open breaker. It needs no key and reveals no secrets or errors.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	ready := true
	if s.Ping != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		body["database"] = "ok"
		if s.Ping(ctx) != nil {
			body["database"], ready = "unavailable", false
		}
	}
	snap, tracker := s.Router.Catalog.Load(), s.Router.Tracker()
	total, available := 0, 0
	for _, d := range snap.Config.Deployments {
		if d.Disabled {
			continue
		}
		total++
		if tracker.Healthy(d.ID) {
			available++
		}
	}
	body["deployments_total"], body["deployments_available"] = total, available
	if available == 0 {
		ready = false
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	body["ready"] = ready
	jsonOut(w, status, body)
}

// requestID accepts a caller's X-Request-ID when it is short and plain,
// otherwise makes one, so a request can be traced through logs.
func requestID(r *http.Request) string {
	id := r.Header.Get("X-Request-ID")
	if len(id) > 0 && len(id) <= 64 {
		ok := true
		for _, c := range id {
			if !(c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				ok = false
				break
			}
		}
		if ok {
			return id
		}
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusWriter records the status code while keeping streaming working.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// traced tags every response with X-Request-ID and logs one line per
// request: method, path, status, duration. Never bodies, prompts or keys.
func (s *Server) traced(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID(r)
		w.Header().Set("X-Request-ID", id)
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		if s.Log != nil && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			s.Log.Info("request", "request_id", id, "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration_ms", time.Since(start).Milliseconds())
		}
	})
}
