package provider

import (
	"errors"
	"strings"
	"testing"
)

func TestSSEEventSizeIsBounded(t *testing.T) {
	oneHugeLine := "data: " + strings.Repeat("x", 2<<20) + "\n\n"
	if _, err := newEventReader(strings.NewReader(oneHugeLine)).Next(); !errors.Is(err, errEventTooLarge) {
		t.Fatalf("2 MiB line: want errEventTooLarge, got %v", err)
	}
	line := "data: " + strings.Repeat("y", 400<<10) + "\n"
	manyLines := line + line + line + "\n"
	if _, err := newEventReader(strings.NewReader(manyLines)).Next(); !errors.Is(err, errEventTooLarge) {
		t.Fatalf("3 × 400 KiB lines: want errEventTooLarge, got %v", err)
	}
}

func TestSSECommentsAndCRLF(t *testing.T) {
	r := newEventReader(strings.NewReader(": keep-alive\r\n\r\nevent: delta\r\ndata: ok\r\n\r\n"))
	e, err := r.Next()
	if err != nil || e.Name != "delta" || e.Data != "ok" {
		t.Fatalf("bad event: %+v %v", e, err)
	}
}
