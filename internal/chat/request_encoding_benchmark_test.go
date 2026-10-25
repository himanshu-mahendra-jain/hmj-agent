package chat

import (
	"crypto/rand"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/config"
)

// benchmarkConversation is a long agentic conversation of about 400K tokens (1.6 MB of
// text): turns of a prompt, an answer with a tool call and a large tool result, 20 of
// which carry a picture of about 200 KB from an image store in dir.
func benchmarkConversation(b *testing.B, dir string) []Message {
	b.Helper()
	SetImageDir(dir)
	b.Cleanup(func() { SetImageDir("") })
	var images []Image
	for i := range 20 {
		data := make([]byte, 200*1024)
		if _, err := rand.Read(data); err != nil {
			b.Fatal(err)
		}
		img, err := StoreImage(data, "image/png", 1000, 800, fmt.Sprintf("shot%d.png", i))
		if err != nil {
			b.Fatal(err)
		}
		images = append(images, img)
	}
	result := strings.Repeat("func example() { return compute(alpha, beta) } // result line\n", 100)
	msgs := []Message{{Role: "system", Content: strings.Repeat("You are a coding agent. ", 400)}}
	for i := range 240 {
		id := fmt.Sprintf("call_%d", i)
		tool := Message{Role: "tool", ToolCallID: id, Content: result}
		if i%12 == 0 && len(images) > 0 {
			tool.Images, images = images[:1], images[1:]
		}
		msgs = append(msgs,
			Message{Role: "user", Content: fmt.Sprintf("Step %d: look at the next file and fix what is wrong with it.", i)},
			Message{Role: "assistant", Content: "Reading it now.", ToolCalls: []ToolCall{{ID: id, Type: "function",
				Function: ToolCallFunction{Name: "read_file", Arguments: fmt.Sprintf(`{"path":"internal/pkg/file%d.go","offset":1,"limit":400}`, i)}}}},
			tool,
		)
	}
	return msgs
}

// benchmarkTools is a tool list the size of hmj's.
func benchmarkTools() []ToolDefinition {
	var defs []ToolDefinition
	for i := range 10 {
		defs = append(defs, ToolDefinition{Type: "function", Function: FunctionDefinition{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: strings.Repeat("Does one thing well. ", 20),
			Parameters: map[string]any{"type": "object", "properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "file path"},
				"mode": map[string]any{"type": "string", "enum": []string{"a", "b", "c"}},
			}, "required": []string{"path"}},
		}})
	}
	return defs
}

func BenchmarkAnthropicRequest400KTokens(b *testing.B) {
	msgs := benchmarkConversation(b, filepath.Join(b.TempDir(), "images"))
	tools := benchmarkTools()
	model := config.Model{ID: "claude-opus-5-5", Effort: "high"}
	b.ReportAllocs()
	for b.Loop() {
		body, err := encodeRequestBody(anthropicRequestFor(model, msgs, tools))
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(body)))
	}
}

func BenchmarkResponsesRequest400KTokens(b *testing.B) {
	msgs := benchmarkConversation(b, filepath.Join(b.TempDir(), "images"))
	tools := benchmarkTools()
	b.ReportAllocs()
	for b.Loop() {
		body, err := encodeRequestBody(ResponsesRequest{Model: "gpt", Input: convertMessagesToInputItems(msgs), Stream: true, Tools: responsesTools(tools)})
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(body)))
	}
}
