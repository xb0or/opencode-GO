// Package stream provides a protocol-agnostic pipe for relaying byte streams
// from an io.Reader to an http.ResponseWriter with SSE-flushing semantics.
//
// The pipe does NOT know about Chat, Messages, Responses, or any specific
// protocol. It only guarantees:
//
//   - Lines are written to the client as soon as they arrive.
//   - The writer is flushed after each line (for SSE/text-event-stream).
//   - The reader is fully consumed and closed.
//
// Usage:
//
//	result := stream.Pipe(w, resp.Body)
//	// result.Bytes   — raw bytes relayed
//	// result.Err     — first error encountered
//	// result.FirstByteMs — milliseconds to first data line
package stream

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"time"
)

// Result holds the outcome of a Pipe relay.
type Result struct {
	Bytes        []byte // raw bytes that were relayed
	Err          error  // first error encountered (read or write)
	FirstByteMs  int64  // milliseconds from start to first data line (0 if never)
	Lines        int    // number of lines relayed
}

// LineHook is an optional callback invoked for each relaid line. It receives
// the raw line (without trailing newline). Returning a non-nil error aborts
// the relay immediately.
type LineHook func(rawLine []byte) error

// Pipe relays a byte stream from src to dst, line by line, with SSE flushing.
//
// If dst implements http.Flusher, it is flushed after each line — this keeps
// text/event-stream responses live without buffering entire chunks.
//
// If hook is non-nil, it is called for each line AFTER the line has been
// written to dst. The hook must not retain or modify the slice — make a copy
// if you need to keep it.
//
// The function blocks until src is fully consumed or an error occurs.
// The caller is responsible for closing src.
func Pipe(dst io.Writer, src io.Reader, start time.Time, hook LineHook) Result {
	flusher, _ := dst.(http.Flusher)

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)

	var result Result
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		line = append(line, '\n')

		result.Bytes = append(result.Bytes, line...)
		result.Lines++

		if result.FirstByteMs == 0 && len(bytes.TrimSpace(bytes.TrimSuffix(line, []byte{'\n'}))) > 0 {
			result.FirstByteMs = max(1, time.Since(start).Milliseconds())
		}

		if _, err := dst.Write(line); err != nil {
			result.Err = err
			return result
		}
		if flusher != nil {
			flusher.Flush()
		}
		if hook != nil {
			if err := hook(scanner.Bytes()); err != nil {
				result.Err = err
				return result
			}
		}
	}
	if err := scanner.Err(); err != nil {
		result.Err = err
	}
	return result
}

// PipeBytes is like Pipe but returns only the relayed bytes and error.
// It is a convenience wrapper for callers that don't need timing metrics.
func PipeBytes(dst io.Writer, src io.Reader) ([]byte, error) {
	r := Pipe(dst, src, time.Now(), nil)
	return r.Bytes, r.Err
}