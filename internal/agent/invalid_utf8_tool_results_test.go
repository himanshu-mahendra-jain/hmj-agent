package agent

import (
	"context"
	"encoding/json/v2"
	"testing"
	"unicode/utf8"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/tools"
)

// bytesTool answers with text that is not valid UTF-8.
type bytesTool struct{}

func (bytesTool) Name() string                    { return "bytes_tool" }
func (bytesTool) Definition() chat.ToolDefinition { return chat.ToolDefinition{} }
func (bytesTool) Execute(context.Context, string) (string, error) {
	return "مرحبا \xd9 end", nil
}

// TestRunToolResultIsValidUTF8 checks that bytes a tool returns that are not UTF-8 become
// U+FFFD, so the tool message can still be encoded as JSON for the request and the save.
func TestRunToolResultIsValidUTF8(t *testing.T) {
	tc := chat.ToolCall{ID: "c1", Function: chat.ToolCallFunction{Name: "bytes_tool", Arguments: "{}"}}
	r := &Runner{Tools: map[string]tools.Tool{"bytes_tool": bytesTool{}}}
	var text string
	captureOutput(t, func() { text, _ = r.runTool(context.Background(), tc, nil) })
	if want := "مرحبا � end"; text != want {
		t.Errorf("got %q, want %q", text, want)
	}
	if !utf8.ValidString(text) {
		t.Fatalf("result is not valid UTF-8: %q", text)
	}
	if _, err := json.Marshal(chat.Message{Role: "tool", ToolCallID: tc.ID, Content: text}); err != nil {
		t.Errorf("the tool message does not encode: %v", err)
	}
}
