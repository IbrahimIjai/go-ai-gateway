package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProvidersReuseConnections(t *testing.T) {
	var opened atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	p := &OpenAI{Key: "k", BaseURL: srv.URL} // no Client: the shared default applies
	for range 3 {
		if _, err := p.Complete(context.Background(), "m", testReq()); err != nil {
			t.Fatal(err)
		}
	}
	if n := opened.Load(); n != 1 {
		t.Fatalf("want 1 connection for 3 calls, got %d", n)
	}
}

func TestStreamIdleTimeoutCutsSilentUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // silent until the gateway gives up
	}))
	defer srv.Close()
	p := &OpenAI{Key: "k", BaseURL: srv.URL, Client: srv.Client(), StreamIdleTimeout: 50 * time.Millisecond}
	st, err := p.Stream(context.Background(), "m", testReq())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if c, err := st.Recv(); err != nil || c.Text != "hi" {
		t.Fatalf("first chunk: %+v %v", c, err)
	}
	start := time.Now()
	_, err = st.Recv()
	if !errors.Is(err, errStreamIdle) || Class(err) != ClassTimeout {
		t.Fatalf("want an idle timeout, got %v", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("idle timeout took %s", waited)
	}
}
