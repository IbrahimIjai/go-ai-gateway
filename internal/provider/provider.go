// Package provider adapts different upstream LLM APIs into core types.
package provider

import (
	"context"
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
