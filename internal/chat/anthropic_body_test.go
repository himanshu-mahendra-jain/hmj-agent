package chat

import (
	"strings"
	"testing"

	"hmj-agent/internal/config"
)

// TestAnthropicRequestBodyIsDeterministic checks that the same request always encodes to the same
// bytes. Without it, the v2 JSON encoder walks each tool schema's map in random order, so the
// bytes differed from run to run.
func TestAnthropicRequestBodyIsDeterministic(t *testing.T) {
	tools := []ToolDefinition{{
		Type: "function",
		Function: FunctionDefinition{
			Name:        "read_file",
			Description: "Read a file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":     map[string]any{"type": "string"},
					"offset":   map[string]any{"type": "integer"},
					"limit":    map[string]any{"type": "integer"},
					"numbered": map[string]any{"type": "boolean"},
				},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
		},
	}}
	model := config.Model{ID: "claude-opus-5-5"}
	messages := []Message{{Role: "user", Content: "hi"}}

	first, err := marshalAnthropicRequest(model, messages, tools)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		got, err := marshalAnthropicRequest(model, messages, tools)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("encoding %d differs from the first:\n%s\n%s", i, first, got)
		}
	}

	// Map keys are sorted, the same order encoding/json v1 used.
	const wantSchema = `"input_schema":{"additionalProperties":false,"properties":{"limit":{"type":"integer"},` +
		`"numbered":{"type":"boolean"},"offset":{"type":"integer"},"path":{"type":"string"}},` +
		`"required":["path"],"type":"object"}`
	if !strings.Contains(string(first), wantSchema) {
		t.Fatalf("tool schema keys are not in sorted order:\n%s", first)
	}
}
