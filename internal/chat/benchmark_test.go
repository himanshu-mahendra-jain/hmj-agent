package chat_test

import (
	"fmt"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
)

// BenchmarkReadSSE_10kEvents measures parsing a stream of 10,000 events.
func BenchmarkReadSSE_10kEvents(b *testing.B) {
	var stream strings.Builder
	for i := range 10000 {
		fmt.Fprintf(&stream, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"token %d\"}\n\n", i)
	}
	data := stream.String()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()

	for b.Loop() {
		count := 0
		err := chat.ReadSSE(strings.NewReader(data), func(eventType, payload string) error {
			count++
			return nil
		})
		if err != nil || count != 10000 {
			b.Fatalf("ReadSSE: err=%v events=%d", err, count)
		}
	}
}
