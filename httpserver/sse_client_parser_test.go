package httpserver

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
)

func TestSSEDecoderFramingAndCursor(t *testing.T) {
	decoder := newSSEDecoder(strings.NewReader("\ufeff: comment\r\n\r\nid: opaque:+/42\r\nevent: message\r\ndata: {\r\ndata: \"text\":\"hello\"}\r\n\r\nretry: 1000\ndata: 2\n\nid:\ndata: 3\n\nid: bad\x00cursor\ndata: 4\n\n"), 1024, "initial")
	frame, err := decoder.next()
	if err != nil || frame.kind != "heartbeat" {
		t.Fatalf("comment = %+v, %v", frame, err)
	}
	for _, want := range []struct{ id, data string }{
		{"opaque:+/42", "{\n\"text\":\"hello\"}"},
		{"opaque:+/42", "2"}, {"", "3"}, {"", "4"},
	} {
		frame, err := decoder.next()
		if err != nil || frame.id != want.id || string(frame.data) != want.data {
			t.Fatalf("frame = %+v, %v; want %+v", frame, err, want)
		}
	}
	if _, err := decoder.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal = %v, want EOF", err)
	}
}

func TestSSEDecoderBareCRDoesNotWaitForNextByte(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	decoder := newSSEDecoder(reader, 9, "")
	done := make(chan error, 1)
	go func() {
		frame, err := decoder.next()
		if err == nil && string(frame.data) != "42" {
			err = errors.New("wrong frame")
		}
		done <- err
	}()
	if _, err := io.WriteString(writer, "data:42\r\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("bare CR dispatch waited for a future byte")
	}
}

func TestSSEDecoderCRLFBlockBudget(t *testing.T) {
	// The LF after each dispatching CR is at most one framing byte outside
	// that block's budget, and must not consume the next event's budget.
	decoder := newSSEDecoder(strings.NewReader("data:1\r\n\r\ndata:2\r\n\r\n"), 9, "")
	for _, want := range []string{"1", "2"} {
		frame, err := decoder.next()
		if err != nil || string(frame.data) != want {
			t.Fatalf("frame = %+v, %v, want %s", frame, err, want)
		}
	}
	if _, err := newSSEDecoder(strings.NewReader("data:1\r\n\r\n"), 8, "").next(); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("CRLF over limit = %v", err)
	}
}

func TestSSEDecoderBoundsAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, wire, code string
		max              int64
	}{
		{"line limit", "data: " + strings.Repeat("x", 100), apperr.CodeInvalidArgument, 32},
		{"block limit", strings.Repeat(": small\n", 10) + "\n", apperr.CodeInvalidArgument, 32},
		{"partial data", "data: 42\n", apperr.CodeUnavailable, 64},
		{"partial line", "data: 42", apperr.CodeUnavailable, 64},
		{"unknown event", "event: another\ndata: 42\n\n", apperr.CodeInvalidArgument, 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newSSEDecoder(strings.NewReader(tc.wire), tc.max, "").next()
			if !apperr.Is(err, tc.code) {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
		})
	}
	decoder := newSSEDecoder(strings.NewReader("data: 42\r\r"), 10, "")
	if frame, err := decoder.next(); err != nil || string(frame.data) != "42" {
		t.Fatalf("bare CR / exact limit = %+v, %v", frame, err)
	}
}
