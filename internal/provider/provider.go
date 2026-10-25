// Package provider adapts different upstream LLM APIs into core types.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"

	"example.com/minirouter/internal/core"
)

type Provider interface {
	Name() string
	Complete(context.Context, string, core.ChatRequest) (core.Completion, error)
	Stream(context.Context, string, core.ChatRequest) (Stream, error)
}

type Stream interface {
	Recv() (core.Chunk, error) // returns io.EOF on a completed stream
	Close() error
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("upstream HTTP %d: %s", e.Status, e.Message) }
func Retryable(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Status == 429 || e.Status == 408 || e.Status >= 500)
}

type sliceStream struct {
	chunks []core.Chunk
	index  int
}

func (s *sliceStream) Recv() (core.Chunk, error) {
	if s.index >= len(s.chunks) {
		return core.Chunk{}, io.EOF
	}
	c := s.chunks[s.index]
	s.index++
	return c, nil
}
func (s *sliceStream) Close() error { return nil }
