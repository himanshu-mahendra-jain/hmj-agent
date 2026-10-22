package chat

import (
	"strings"
	"testing"
)

// TestRequestBodiesAreDeterministic checks that each provider's request encoding gives the same
// bytes on every call, with map keys in sorted order. Tool schemas and replayed tool-call arguments
// are maps, so without sorting their key order changes from run to run.
func TestRequestBodiesAreDeterministic(t *testing.T) {
	schema := func() map[string]any {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":     map[string]any{"type": "string"},
				"offset":   map[string]any{"type": "integer"},
				"limit":    map[string]any{"type": "integer"},
				"numbered": map[string]any{"type": "boolean"},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		}
	}
	// wantOrder is a fragment of the sorted encoding that must appear in the body.
	const schemaOrder = `"additionalProperties":false,"properties":{"limit":`
	cases := []struct {
		name      string
		v         any
		wantOrder string
	}{
		{"responses tool", ToolDefinition{Type: "function", Function: FunctionDefinition{Name: "read_file", Description: "Read a file.", Parameters: schema()}}, schemaOrder},
		{"chat completions tool", completionsTool{Type: "function", Function: FunctionDefinition{Name: "read_file", Description: "Read a file.", Parameters: schema()}}, schemaOrder},
		{"gemini tool", geminiTool{Type: "function", Name: "read_file", Description: "Read a file.", Parameters: schema()}, schemaOrder},
		{"gemini tool-call arguments", map[string]any{"path": "a.txt", "offset": 1, "limit": 5, "numbered": true}, `{"limit":5,"numbered":true,"offset":1,"path":"a.txt"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := encodeRequestBody(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			for i := range 50 {
				got, err := encodeRequestBody(tc.v)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(first) {
					t.Fatalf("encoding %d differs from the first:\n%s\n%s", i, first, got)
				}
			}
			if !strings.Contains(string(first), tc.wantOrder) {
				t.Fatalf("keys are not in sorted order, want %s in:\n%s", tc.wantOrder, first)
			}
		})
	}
}
