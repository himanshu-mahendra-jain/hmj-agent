package chat_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/config"
	"hmj-agent/v2/internal/tools"
)

// TestToolPropertiesGoOutInTheirOrder sends the agent's tools to each provider and checks that
// every tool's properties, and edit_file's edits, arrive in the order the tool declares rather
// than sorted, and that the order's own key is not sent.
func TestToolPropertiesGoOutInTheirOrder(t *testing.T) {
	var defs []chat.ToolDefinition
	for _, tl := range tools.DefaultTools() {
		defs = append(defs, tl.Definition())
	}
	for _, tc := range []struct {
		api, base, stream string
		schema            func(tool jsontext.Value) jsontext.Value
	}{
		{config.APIAnthropic, "", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			func(tool jsontext.Value) jsontext.Value { return member(t, tool, "input_schema") }},
		{config.APIResponses, "", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
			func(tool jsontext.Value) jsontext.Value { return member(t, tool, "parameters") }},
		{config.APICompletions, "", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
			func(tool jsontext.Value) jsontext.Value { return member(t, member(t, tool, "function"), "parameters") }},
		{config.APIGemini, "/v1beta", "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"status\":\"completed\"}}\n\n",
			func(tool jsontext.Value) jsontext.Value { return member(t, tool, "parameters") }},
	} {
		t.Run(tc.api, func(t *testing.T) {
			var body []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.stream)
			}))
			defer server.Close()
			cfg := config.Config{Providers: map[string]config.Provider{
				"p": {BaseURL: server.URL + tc.base, APIKey: "k", API: tc.api, Models: []config.Model{{ID: "m"}}},
			}}
			if _, _, err := chat.NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"},
				[]chat.Message{{Role: "user", Content: "hi"}}, defs, func(string) {}, func(string) {}); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte(chat.PropertyOrder)) {
				t.Errorf("the request carries %q", chat.PropertyOrder)
			}
			var req struct {
				Tools []jsontext.Value `json:"tools"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatal(err)
			}
			if len(req.Tools) != len(defs) {
				t.Fatalf("sent %d tools, want %d", len(req.Tools), len(defs))
			}
			for i, def := range defs {
				schema := tc.schema(req.Tools[i])
				want, _ := def.Function.Parameters[chat.PropertyOrder].([]string)
				if got := propertyNames(t, schema); !slices.Equal(got, want) {
					t.Errorf("%s: properties sent as %v, want %v", def.Function.Name, got, want)
				}
				if def.Function.Name == "edit_file" {
					items := member(t, member(t, member(t, schema, "properties"), "edits"), "items")
					if got := propertyNames(t, items); strings.Join(got, " ") != "old_string new_string replace_all" {
						t.Errorf("edit_file: edits sent as %v, want old_string new_string replace_all", got)
					}
				}
			}
		})
	}
}

// member returns the value of an object's member.
func member(t *testing.T, obj jsontext.Value, name string) jsontext.Value {
	t.Helper()
	var m map[string]jsontext.Value
	if err := json.Unmarshal(obj, &m); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m[name]
}

// propertyNames returns a schema's property names in the order they were sent.
func propertyNames(t *testing.T, schema jsontext.Value) []string {
	t.Helper()
	dec := jsontext.NewDecoder(bytes.NewReader(member(t, schema, "properties")))
	if _, err := dec.ReadToken(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for dec.PeekKind() == '"' {
		name, err := dec.ReadToken()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name.String())
		if err := dec.SkipValue(); err != nil {
			t.Fatal(err)
		}
	}
	return names
}
