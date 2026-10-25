package provider

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

type event struct {
	Name string
	Data string
}
type eventReader struct{ r *bufio.Reader }

func newEventReader(r io.Reader) *eventReader {
	return &eventReader{r: bufio.NewReaderSize(r, 32*1024)}
}
func (r *eventReader) Next() (event, error) {
	var e event
	var data []string
	for {
		line, err := r.r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return event{}, err
		}
		if err == io.EOF && line == "" {
			if len(data) > 0 {
				e.Data = strings.Join(data, "\n")
				return e, nil
			}
			return event{}, io.EOF
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if len(data) > 0 {
				e.Data = strings.Join(data, "\n")
				return e, nil
			}
			if err == io.EOF {
				return event{}, io.EOF
			}
			e = event{}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			e.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimPrefix(line, "data:")
			d = strings.TrimPrefix(d, " ")
			data = append(data, d)
		}
		if err == io.EOF {
			if len(data) > 0 {
				e.Data = strings.Join(data, "\n")
				return e, nil
			}
			return event{}, io.EOF
		}
	}
}
