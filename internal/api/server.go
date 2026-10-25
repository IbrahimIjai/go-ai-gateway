// Package api provides the OpenAI-compatible HTTP surface.
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
	"example.com/minirouter/internal/catalog"
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
	mux.HandleFunc("POST /v1/route/explain", s.auth(s.explain))
	return mux
}
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The scheme is case-insensitive (RFC 7235); a bare key is not a bearer token.
		scheme, supplied, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(supplied), []byte(s.Key)) != 1 {
			jsonError(w, http.StatusUnauthorized, "invalid_api_key", "Missing or invalid bearer key")
			return
		}
		next(w, r)
	}
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	snap := s.Router.Catalog.Load()
	data := make([]any, 0, len(snap.Models)+len(snap.Aliases))
	for _, m := range snap.Models {
		item := map[string]any{"id": m.ID, "object": "model", "owned_by": "minirouter",
			"capabilities": m.Capabilities, "context_length": nullIfZero(m.ContextTokens), "max_output_tokens": nullIfZero(m.MaxOutputTokens)}
		var deployments []any
		for _, d := range snap.Deployments(m.ID) {
			deployments = append(deployments, map[string]any{"id": d.ID, "provider": d.Provider, "pricing": wirePrice(d.Price)})
		}
		item["deployments"] = deployments
		data = append(data, item)
	}
	for _, a := range snap.Aliases {
		data = append(data, map[string]any{"id": a.ID, "object": "model", "owned_by": "minirouter", "policy": a.Policy, "models": a.Models})
	}
	jsonOut(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// wirePrice publishes a deployment's rates in USD per million tokens. Unknown
// prices are null; unverified ones are marked as drafts, never billed.
func wirePrice(p *catalog.PriceCard) any {
	if p == nil {
		return nil
	}
	return map[string]any{"unit": "usd_per_million_tokens", "input": p.Input, "output": p.Output,
		"cache_read": p.CacheRead, "cache_write": p.CacheWrite, "verified": p.VerifiedAt != "", "source_url": p.SourceURL}
}

func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, http.StatusOK, s.Store.Summary())
}

// decodeChat reads and validates a chat request, answering 400 itself.
func decodeChat(w http.ResponseWriter, r *http.Request) (core.ChatRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var wire chatRequest
	if err := dec.Decode(&wire); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON or unsupported field: "+err.Error())
		return core.ChatRequest{}, false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", "send exactly one JSON object")
		return core.ChatRequest{}, false
	}
	req, err := wire.toCore()
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return core.ChatRequest{}, false
	}
	return req, true
}

// explain answers how a chat request would be routed, without calling any
// provider: gates, policy scores and the deployment order.
func (s *Server) explain(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeChat(w, r)
	if !ok {
		return
	}
	d, plan, err := s.Router.Explain(req)
	if err != nil {
		s.fail(w, err)
		return
	}
	order := make([]string, len(plan))
	for i, c := range plan {
		order[i] = c.Deployment.ID
	}
	jsonOut(w, http.StatusOK, map[string]any{"decision": d, "plan": order})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeChat(w, r)
	if !ok {
		return
	}
	id := newID()
	now := time.Now().Unix()
	if req.Stream {
		s.stream(w, r, req, id, now)
		return
	}
	c, out, route, err := s.Router.Complete(r.Context(), req)
	routeHeaders(w, c, route)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(id, c, route, out.Usage, false)
	jsonOut(w, http.StatusOK, map[string]any{
		"id": id, "object": "chat.completion", "created": now, "model": c.Model.ID,
		"choices": []any{map[string]any{"index": 0, "message": wireMessage(out), "finish_reason": fallbackFinish(out.FinishReason)}},
		"usage":   wireUsage(out.Usage),
	})
}
func (s *Server) stream(w http.ResponseWriter, r *http.Request, req core.ChatRequest, id string, now int64) {
	c, stream, route, err := s.Router.Stream(r.Context(), req)
	routeHeaders(w, c, route)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer stream.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	f, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "internal_error", "streaming not supported")
		return
	}
	write := func(choices []any, usage *core.Usage) error {
		raw, err := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": now, "model": c.Model.ID, "choices": choices, "usage": wireUsage(usage)})
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
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			failed = true
			if s.Log != nil {
				s.Log.Warn("upstream streaming interrupted", "model", c.Model.ID, "deployment", c.Deployment.ID, "error", err)
			}
			// No [DONE] marker: the stream is incomplete and its cost is unknown.
			errorPayload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": "upstream stream interrupted", "type": "upstream_error"}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", errorPayload)
			f.Flush()
			break
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
		if delta := wireDelta(chunk); delta != nil {
			if err := write([]any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}, nil); err != nil {
				failed = true
				break
			}
		}
	}
	s.record(id, c, route, usage, failed)
	if failed {
		return
	}
	if err := write([]any{map[string]any{"index": 0, "delta": map[string]string{}, "finish_reason": fallbackFinish(finish)}}, nil); err != nil {
		return
	}
	if req.IncludeUsage {
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
	var none *router.NoEligibleError
	if errors.As(err, &none) {
		jsonError(w, http.StatusBadRequest, "no_eligible_model", none.Error())
		return
	}
	class := provider.Class(err)
	var upstream *provider.APIError
	isUpstream := errors.As(err, &upstream)
	if !isUpstream && class != provider.ClassAdmission && s.Log != nil {
		s.Log.Error("gateway request failure", "class", class, "error", err)
	}
	switch {
	case class == provider.ClassUnsupported:
		jsonError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
	case class == provider.ClassAdmission:
		jsonError(w, http.StatusServiceUnavailable, "upstream_unavailable", "no deployment can take this request right now; try again later")
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

// record journals one served request. A cut stream or missing usage is
// recorded as unavailable, never as zero.
func (s *Server) record(id string, c router.Candidate, route core.RouteDecision, usage *core.Usage, failed bool) {
	r := core.UsageRecord{ID: id, Model: c.Model.ID, Provider: c.Deployment.Provider, Deployment: c.Deployment.ID, Status: "ok", Usage: usage, Route: &route}
	if failed || usage == nil {
		r.Status, r.Usage = "usage_unavailable", nil
	}
	s.Store.Add(r, c.Price)
}

// routeHeaders explains the routing, on failures too.
func routeHeaders(w http.ResponseWriter, c router.Candidate, route core.RouteDecision) {
	for k, v := range router.Headers(route) {
		w.Header().Set(k, v)
	}
	if c.Deployment.Provider != "" {
		w.Header().Set("X-MiniRouter-Provider", c.Deployment.Provider)
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
