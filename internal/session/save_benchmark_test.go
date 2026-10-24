package session_test

import (
	"fmt"
	"strings"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/session"
)

// BenchmarkSave saves a session of five conversations of 200 KB each, about 1 MB, to real
// files with their fsync and rename. Only the active conversation and the state are written.
func BenchmarkSave(b *testing.B) {
	sm := session.LoadSessionManager(b.TempDir(), testModelRef())
	result := strings.Repeat("a line of tool output that the model read\n", 50)
	for c := range 5 {
		conv := sm.Active()
		if c > 0 {
			conv = sm.New(fmt.Sprintf("conv %d", c))
		}
		for i := range 100 {
			conv.Messages = append(conv.Messages,
				chat.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
				chat.Message{Role: "tool", ToolCallID: fmt.Sprint(i), Content: result})
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := sm.Save(); err != nil {
			b.Fatal(err)
		}
	}
}
