package api

import (
	"testing"
	"time"
)

func TestFinalizeResponseTimingFillsSuccessfulStreamFRT(t *testing.T) {
	timing := finalizeResponseTiming(responseTiming{TTFTMs: 12}, true, 200, 350*time.Millisecond)
	if timing.FirstResponseMs != 350 {
		t.Fatalf("expected fallback FRT 350ms, got %d", timing.FirstResponseMs)
	}
	if timing.TTFTMs != 12 {
		t.Fatalf("expected TTFT to remain 12ms, got %d", timing.TTFTMs)
	}
}

func TestFinalizeResponseTimingDoesNotFillErrorsOrNonStreams(t *testing.T) {
	for _, test := range []struct {
		name   string
		stream bool
		status int
	}{
		{name: "error", stream: true, status: 500},
		{name: "non-stream", stream: false, status: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			timing := finalizeResponseTiming(responseTiming{}, test.stream, test.status, 350*time.Millisecond)
			if timing.FirstResponseMs != 0 {
				t.Fatalf("expected empty FRT, got %d", timing.FirstResponseMs)
			}
		})
	}
}
