package provider

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// maxEventBytes bounds one SSE line and one assembled event, so a broken or
// hostile upstream cannot grow gateway memory without limit. Text deltas are
// far smaller; multimodal output will need a larger bound.
const maxEventBytes = 1 << 20

var errEventTooLarge = errors.New("upstream SSE event exceeds 1 MiB")

type event struct {
	Name string
	Data string
}
type eventReader struct{ s *bufio.Scanner }

func newEventReader(r io.Reader) *eventReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 32*1024), maxEventBytes)
	return &eventReader{s: s}
}

// Next returns the next event with data. Comments, ids and retry fields are
// skipped; a final event without its blank line is still returned.
func (r *eventReader) Next() (event, error) {
	var e event
	var data []string
	size := 0
	for r.s.Scan() {
		line := r.s.Text() // CRLF already trimmed
		if line == "" {
			if len(data) > 0 {
				e.Data = strings.Join(data, "\n")
				return e, nil
			}
			e = event{}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			e.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if size += len(d) + 1; size > maxEventBytes {
				return event{}, errEventTooLarge
			}
			data = append(data, d)
		}
	}
	if err := r.s.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return event{}, errEventTooLarge
		}
		return event{}, err
	}
	if len(data) > 0 {
		e.Data = strings.Join(data, "\n")
		return e, nil
	}
	return event{}, io.EOF
}
