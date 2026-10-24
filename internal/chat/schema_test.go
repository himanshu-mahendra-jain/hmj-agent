package chat

import "testing"

// TestSchemaSendsPropertiesInTheirOrder checks that a schema's properties go out in its
// PropertyOrder, nested objects' too, with any it leaves out after them, sorted, that every other
// object keeps its keys sorted, and that the PropertyOrder key itself is not sent.
func TestSchemaSendsPropertiesInTheirOrder(t *testing.T) {
	schema := Schema{
		"type":        "object",
		PropertyOrder: []string{"path", "old_string", "new_string", "edits", "gone"},
		"properties": map[string]any{
			"new_string": map[string]any{"type": "string"},
			"old_string": map[string]any{"type": "string"},
			"path":       map[string]any{"type": "string", "description": "d"},
			"edits": map[string]any{"type": "array", "items": map[string]any{
				PropertyOrder: []string{"old_string", "new_string"},
				"properties": map[string]any{
					"new_string": map[string]any{"type": "string"},
					"old_string": map[string]any{"type": "string"},
				},
			}},
			"replace_all": map[string]any{"type": "boolean"},
			"by_regexp":   map[string]any{"type": "boolean"},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
	body, err := encodeRequestBody(struct {
		Tools []FunctionDefinition `json:"tools"`
	}{[]FunctionDefinition{{Name: "edit_file", Parameters: schema}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tools":[{"name":"edit_file","parameters":{"additionalProperties":false,"properties":{` +
		`"path":{"description":"d","type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},` +
		`"edits":{"items":{"properties":{"old_string":{"type":"string"},"new_string":{"type":"string"}}},"type":"array"},` +
		`"by_regexp":{"type":"boolean"},"replace_all":{"type":"boolean"}},"required":["path"],"type":"object"}}]}`
	if string(body) != want {
		t.Errorf("encoded\n%s\nwant\n%s", body, want)
	}
}
