package stream

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPipeRelaysAllLines(t *testing.T) {
	// Each \n is a line terminator. "a\n\nb\n" has 3 lines: "a", "", "b".
	input := "data: line1\n\ndata: line2\n\ndata: [DONE]\n"
	src := strings.NewReader(input)
	var dst bytes.Buffer

	result := Pipe(&dst, src, time.Now(), nil)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	// 5 lines: data: line1, (empty), data: line2, (empty), data: [DONE]
	if result.Lines != 5 {
		t.Fatalf("expected 5 lines (including SSE blank separators), got %d", result.Lines)
	}
	if dst.String() != input {
		t.Fatalf("output doesn't match input:\nwant: %q\ngot:  %q", input, dst.String())
	}
}

func TestPipeRecordsFirstByteTiming(t *testing.T) {
	input := "data: hello\n\n"
	src := strings.NewReader(input)
	var dst bytes.Buffer

	start := time.Now()
	result := Pipe(&dst, src, start, nil)

	if result.FirstByteMs == 0 {
		t.Fatal("FirstByteMs should be non-zero for non-empty input")
	}
}

func TestPipeEmptyInput(t *testing.T) {
	src := strings.NewReader("")
	var dst bytes.Buffer

	result := Pipe(&dst, src, time.Now(), nil)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Lines != 0 {
		t.Fatalf("expected 0 lines, got %d", result.Lines)
	}
	if result.FirstByteMs != 0 {
		t.Fatalf("FirstByteMs should be 0 for empty input, got %d", result.FirstByteMs)
	}
}

func TestPipeWriteErrorStopsRelay(t *testing.T) {
	src := strings.NewReader("line1\nline2\nline3\n")
	dst := &errWriter{err: errors.New("write failed")}

	result := Pipe(dst, src, time.Now(), nil)

	if result.Err == nil {
		t.Fatal("expected error from failed write")
	}
	if result.Err.Error() != "write failed" {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	// Should have stopped after first failed write
	if result.Lines != 1 {
		t.Fatalf("expected 1 line before error, got %d", result.Lines)
	}
}

func TestPipeFlushesHTTPResponseWriter(t *testing.T) {
	input := "data: flushed\n\n"
	src := strings.NewReader(input)

	rec := httptest.NewRecorder()
	result := Pipe(rec, src, time.Now(), nil)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if rec.Body.String() != input {
		t.Fatalf("output mismatch: want %q, got %q", input, rec.Body.String())
	}
}

func TestPipeBytes(t *testing.T) {
	input := "hello\nworld\n"
	src := strings.NewReader(input)
	var dst bytes.Buffer

	out, err := PipeBytes(&dst, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != input {
		t.Fatalf("output mismatch: want %q, got %q", input, string(out))
	}
}

// errWriter is an io.Writer that always returns an error.
type errWriter struct{ err error }

func (w *errWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

// Ensure PipeBytes works with io.Reader/Writer interfaces.
var _ io.Writer = (*bytes.Buffer)(nil)