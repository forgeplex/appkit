package httpserver

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/internal/metrics"
)

type sseClientFrame struct {
	kind string
	id   string
	data []byte
}

// The decoder bounds raw blocks before allocating a decoded JSON value. A
// single bufio buffer and one bounded in-flight block are independent of the
// bounded Local Stream queue. Cursor state follows SSE: omitted IDs inherit,
// an empty ID clears it, and an ID containing NUL is ignored.
type sseDecoder struct {
	reader   *bufio.Reader
	maxBytes int64
	id       string
	first    bool
	skipLF   bool
	blockLF  bool
}

func newSSEDecoder(reader io.Reader, maxBytes int64, initialID string) *sseDecoder {
	return &sseDecoder{reader: bufio.NewReader(reader), maxBytes: maxBytes, id: initialID, first: true}
}

func (d *sseDecoder) next() (sseClientFrame, error) {
	var data []byte
	var event string
	var size int64
	hasData, hasComment := false, false
	for {
		line, err := d.line(&size)
		if err != nil {
			if errors.Is(err, io.EOF) && (hasData || len(line) != 0) {
				return sseClientFrame{}, apperr.Unavailable(nil)
			}
			return sseClientFrame{}, err
		}
		if d.first {
			d.first = false
			line = strings.TrimPrefix(line, "\ufeff")
		}
		if !utf8.ValidString(line) {
			return sseClientFrame{}, apperr.InvalidArgument("SSE event must be UTF-8")
		}
		if line == "" {
			d.blockLF = d.skipLF
			if !hasData {
				kind := metrics.OutcomeOther
				if hasComment {
					kind = metrics.FrameHeartbeat
				}
				return sseClientFrame{kind: kind}, nil
			}
			kind := metrics.FrameData
			switch event {
			case "", "message":
			case sseErrorEvent:
				kind = metrics.FrameError
			default:
				return sseClientFrame{}, apperr.InvalidArgument("unsupported SSE event type")
			}
			return sseClientFrame{kind: kind, id: d.id, data: bytes.TrimSuffix(data, []byte{'\n'})}, nil
		}
		if strings.HasPrefix(line, ":") {
			hasComment = true
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			hasData = true
			data = append(data, value...)
			data = append(data, '\n')
		case "id":
			if !strings.ContainsRune(value, 0) {
				d.id = value
			}
		}
	}
}

// SSE permits LF, CRLF and bare CR line delimiters. Defer consuming the LF
// after CR until the next read so a flushed CR delimiter never waits for bytes
// belonging to the next event. Only the optional LF after a dispatching CR is
// framing overhead outside the event budget; it cannot accumulate across lines.
func (d *sseDecoder) line(size *int64) (string, error) {
	var line []byte
	for {
		b, err := d.reader.ReadByte()
		if err != nil {
			return string(line), err
		}
		if d.skipLF {
			d.skipLF = false
			if b == '\n' {
				if d.blockLF {
					d.blockLF = false
					continue
				}
				*size += 1
				if *size > d.maxBytes {
					return "", apperr.InvalidArgument("SSE event exceeds the configured wire limit")
				}
				continue
			}
		}
		d.blockLF = false
		*size += 1
		if *size > d.maxBytes {
			return "", apperr.InvalidArgument("SSE event exceeds the configured wire limit")
		}
		switch b {
		case '\r':
			d.skipLF = true
			return string(line), nil
		case '\n':
			return string(line), nil
		default:
			line = append(line, b)
		}
	}
}
