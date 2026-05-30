package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestScanner_BasicEvents ensures the incremental scanner emits the
// same events as Parse for a small canonical Anthropic-shaped stream.
// We don't try to assert byte-identical Offset values — those depend
// on the consumer's read pattern — but the field+data extraction
// must match.
func TestScanner_BasicEvents(t *testing.T) {
	body := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")

	s := NewScanner(strings.NewReader(body))
	var got []string
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, ev.Name)
	}
	want := []string{"message_start", "content_block_delta", "message_stop"}
	if !equalStr(got, want) {
		t.Errorf("event names = %v, want %v", got, want)
	}
	if !s.StreamDone() {
		t.Errorf("StreamDone = false; expected true after message_stop")
	}
}

// TestScanner_DataOnlyDONE swallows the `data: [DONE]` terminator
// without surfacing it as an event (matches Parse semantics).
func TestScanner_DataOnlyDONE(t *testing.T) {
	body := "data: {\"x\":1}\n\ndata: [DONE]\n\n"
	s := NewScanner(strings.NewReader(body))
	ev, err := s.Next()
	if err != nil {
		t.Fatalf("Next1: %v", err)
	}
	if ev.Name != "" {
		t.Errorf("ev name = %q, want empty (data-only frame)", ev.Name)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF after [DONE]; got %v", err)
	}
	if !s.StreamDone() {
		t.Error("StreamDone should be true after [DONE]")
	}
}

// TestWriteEvent_RoundTrip ensures WriteEvent → NewScanner → Next
// round-trips one event byte-for-byte (modulo data normalization).
func TestWriteEvent_RoundTrip(t *testing.T) {
	in := Event{Name: "content_block_delta", Data: []byte(`{"x":42}`)}
	var buf bytes.Buffer
	if err := WriteEvent(&buf, in); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	s := NewScanner(&buf)
	ev, err := s.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Name != in.Name {
		t.Errorf("Name round-trip: got %q, want %q", ev.Name, in.Name)
	}
	if !bytes.Equal(ev.Data, in.Data) {
		t.Errorf("Data round-trip: got %s, want %s", ev.Data, in.Data)
	}
}

// TestWriteEvent_NoName produces a data-only frame (Chat-shape).
func TestWriteEvent_NoName(t *testing.T) {
	in := Event{Data: []byte(`{"choices":[{"delta":{"content":"hi"}}]}`)}
	var buf bytes.Buffer
	if err := WriteEvent(&buf, in); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "event:") {
		t.Errorf("empty Name should not emit event: line; got %q", got)
	}
	if !strings.Contains(got, "data: {\"choices\"") {
		t.Errorf("data line missing; got %q", got)
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
