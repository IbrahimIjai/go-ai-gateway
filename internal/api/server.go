// Package api provides the text-only OpenAI-compatible HTTP surface.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
	"example.com/minirouter/internal/router"
)

type Server struct {
	Router *router.Router
	Store  *billing.Store
	Key    string
	Log    *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/models", s.auth(s.models))
	mux.HandleFunc("GET /v1/usage", s.auth(s.usage))
	mux.HandleFunc("POST /v1/chat/completions", s.auth(s.chat))
	return mux
}
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(supplied) != len(s.Key) || subtle.ConstantTimeCompare([]byte(supplied), []byte(s.Key)) != 1 {
			jsonError(w, http.StatusUnauthorized, "invalid_api_key", "Missing or invalid bearer key")
			return
		}
		next(w, r)
	}
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]item, 0)
	for _, m := range s.Router.List() {
		owner := m.Provider
		if owner == "" {
			owner = "minirouter"
		}
		data = append(data, item{ID: m.ID, Object: "model", OwnedBy: owner})
	}
	jsonOut(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, http.StatusOK, s.Store.Summary())
}
func validate(req core.ChatRequest) error {
	if req.Model == "" {
		return errors.New("model is required")
	}
	if len(req.Messages) == 0 || len(req.Messages) > 100 {
		return errors.New("messages must contain 1–100 entries")
	}
	if req.MaxCompletionTokens != nil && req.MaxTokens != nil {
		return errors.New("use only one of max_tokens and max_completion_tokens")
	}
	if limit := req.TokenLimit(); limit < 1 || limit > 8192 {
		return errors.New("max output tokens must be between 1 and 8192")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return errors.New("temperature must be between 0 and 2")
	}
	if req.StreamOptions != nil && !req.Stream {
		return errors.New("stream_options requires stream=true")
	}
	hasUser := false
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" && m.Role != "developer" {
			return fmt.Errorf("unsupported role: %q", m.Role)
		}
		if m.Role == "user" {
			hasUser = true
		}
		if len(m.Content) > 100_000 {
			return errors.New("message content exceeds 100KB")
		}
	}
	if !hasUser {
		return errors.New("at least one user message is required")
	}
	return nil
}
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req core.ChatRequest
	if err := dec.Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON or unsupported field: "+err.Error())
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", "send exactly one JSON object")
		return
	}
	if err := validate(req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	id := newID()
	now := time.Now().Unix()
	if req.Stream {
		s.stream(w, r, req, id, now)
		return
	}
	m, out, err := s.Router.Complete(r.Context(), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	if out.Usage == nil {
		s.Store.Add(core.UsageRecord{ID: id, Model: m.ID, Provider: m.Provider, Status: "usage_unavailable"}, m)
	} else {
		s.Store.Add(core.UsageRecord{ID: id, Model: m.ID, Provider: m.Provider, Status: "ok", Usage: out.Usage}, m)
	}
	w.Header().Set("X-MiniRouter-Provider", m.Provider)
	w.Header().Set("X-MiniRouter-Resolved-Model", m.ID)
	jsonOut(w, http.StatusOK, map[string]any{
		"id": id, "object": "chat.completion", "created": now, "model": m.ID,
		"choices": []any{map[string]any{"index": 0, "message": core.Message{Role: "assistant", Content: out.Text}, "finish_reason": fallbackFinish(out.FinishReason)}},
		"usage":   out.Usage,
	})
}
func (s *Server) stream(w http.ResponseWriter, r *http.Request, req core.ChatRequest, id string, now int64) {
	m, stream, err := s.Router.Stream(r.Context(), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-MiniRouter-Provider", m.Provider)
	w.Header().Set("X-MiniRouter-Resolved-Model", m.ID)
	f, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "internal_error", "streaming not supported")
		return
	}
	write := func(choices []any, usage *core.Usage) error {
		raw, err := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": now, "model": m.ID, "choices": choices, "usage": usage})
		if err != nil {
			return err
		}
		if _, err = w.Write([]byte("data: " + string(raw) + "\n\n")); err != nil {
			return err
		}
		f.Flush()
		return nil
	}
	if err := write([]any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil}}, nil); err != nil {
		return
	}
	var usage *core.Usage
	var finish string
	failed := false
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			failed = true
			if s.Log != nil {
				s.Log.Warn("upstream streaming interrupted", "model", m.ID, "error", err)
			}
			// No [DONE] marker: the stream is incomplete and its cost is unknown.
			errorPayload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": "upstream stream interrupted", "type": "upstream_error"}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", errorPayload)
			f.Flush()
			break
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if c.Text != "" {
			if err := write([]any{map[string]any{"index": 0, "delta": map[string]string{"content": c.Text}, "finish_reason": nil}}, nil); err != nil {
				failed = true
				break
			}
		}
	}
	if failed || usage == nil {
		s.Store.Add(core.UsageRecord{ID: id, Model: m.ID, Provider: m.Provider, Status: "usage_unavailable"}, m)
	} else {
		s.Store.Add(core.UsageRecord{ID: id, Model: m.ID, Provider: m.Provider, Status: "ok", Usage: usage}, m)
	}
	if failed {
		return
	}
	if err := write([]any{map[string]any{"index": 0, "delta": map[string]string{}, "finish_reason": fallbackFinish(finish)}}, nil); err != nil {
		return
	}
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		if err := write([]any{}, usage); err != nil {
			return
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	f.Flush()
}
func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, router.ErrUnknownModel) {
		jsonError(w, http.StatusNotFound, "model_not_found", "requested model is not available")
		return
	}
	class := provider.Class(err)
	var upstream *provider.APIError
	isUpstream := errors.As(err, &upstream)
	if !isUpstream && s.Log != nil {
		s.Log.Error("gateway request failure", "class", class, "error", err)
	}
	switch {
	case class == provider.ClassQuota:
		jsonError(w, http.StatusBadGateway, "upstream_error", "provider account quota or credit is exhausted; retrying will not help")
	case class == provider.ClassCredential:
		jsonError(w, http.StatusBadGateway, "upstream_error", "provider rejected the gateway's credentials")
	case class == provider.ClassTimeout:
		jsonError(w, http.StatusGatewayTimeout, "upstream_timeout", "provider timed out")
	case class == provider.ClassTransient, class == provider.ClassRateLimit:
		jsonError(w, http.StatusBadGateway, "upstream_error", "provider unavailable; try again later")
	case isUpstream:
		jsonError(w, http.StatusBadGateway, "upstream_error", "provider rejected request (HTTP "+fmt.Sprint(upstream.Status)+")")
	default:
		jsonError(w, http.StatusBadGateway, "upstream_error", "provider request failed")
	}
}
func fallbackFinish(s string) string {
	if s == "" {
		return "stop"
	}
	return s
}
func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-mini-" + hex.EncodeToString(b[:])
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func jsonError(w http.ResponseWriter, status int, kind, message string) {
	jsonOut(w, status, map[string]any{"error": map[string]string{"type": kind, "message": message}})
}
