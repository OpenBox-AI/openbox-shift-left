package claudecode

import (
	"bytes"
	"fmt"
	"testing"
)

// largeTranscript is the shape the phase is about: Claude Code's transcript
// JSONL is unbounded, a long session is megabytes of it, and this runs on the
// hook path at SessionEnd.
func largeTranscript(lines int) []byte {
	var b bytes.Buffer
	for i := range lines {
		fmt.Fprintf(&b, `{"type":"assistant","timestamp":"2026-08-31T12:%02d:%02dZ","message":{"role":"assistant","usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":128,"cache_read_input_tokens":4096},"content":[{"type":"text","text":"%s"}]}}`+"\n",
			i%60, (i*7)%60, i, i*2, bytes.Repeat([]byte("x"), 200))
	}
	return b.Bytes()
}

func BenchmarkAggregateUsage(b *testing.B) {
	for _, lines := range []int{1_000, 20_000} {
		raw := largeTranscript(lines)
		b.Run(fmt.Sprintf("%d lines/%dKiB", lines, len(raw)>>10), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := aggregateUsage(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkAggregateTurnWindow(b *testing.B) {
	raw := largeTranscript(20_000)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		aggregateTurnWindow(raw, false)
	}
}
