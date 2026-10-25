package tools

import (
	"testing"

	"hmj-agent/v2/internal/chat"
)

// TestToolSchemasAreClosed checks that every built-in tool schema is a closed object
// (additionalProperties false) with a required list present, and names only declared properties as
// required.
func TestToolSchemasAreClosed(t *testing.T) {
	for _, tool := range DefaultTools() {
		def := tool.Definition()
		params := def.Function.Parameters
		if params == nil {
			t.Errorf("%s: no parameters", def.Function.Name)
			continue
		}
		if ap, ok := params["additionalProperties"].(bool); !ok || ap {
			t.Errorf("%s: additionalProperties = %v, want false", def.Function.Name, params["additionalProperties"])
		}
		required, ok := params["required"].([]string)
		if !ok {
			t.Errorf("%s: required = %#v, want a present list", def.Function.Name, params["required"])
			continue
		}
		props, _ := params["properties"].(map[string]any)
		for _, name := range required {
			if _, declared := props[name]; !declared {
				t.Errorf("%s: required property %q is not declared", def.Function.Name, name)
			}
		}
	}
}

// TestToolSchemasDeclareTheirPropertyOrder checks that every object in every built-in tool schema
// lists each of its properties once in its chat.PropertyOrder; one left out would be sent after
// the others, sorted.
func TestToolSchemasDeclareTheirPropertyOrder(t *testing.T) {
	var check func(tool, at string, schema map[string]any)
	check = func(tool, at string, schema map[string]any) {
		if items, ok := schema["items"].(map[string]any); ok {
			check(tool, at+".items", items)
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			return
		}
		order, _ := schema[chat.PropertyOrder].([]string)
		listed := map[string]bool{}
		for _, name := range order {
			if _, declared := props[name]; !declared || listed[name] {
				t.Errorf("%s%s: %s lists %q, which is not a property or is listed twice", tool, at, chat.PropertyOrder, name)
			}
			listed[name] = true
		}
		for name, p := range props {
			if !listed[name] {
				t.Errorf("%s%s: property %q is missing from %s %v", tool, at, name, chat.PropertyOrder, order)
			}
			if sub, ok := p.(map[string]any); ok {
				check(tool, at+"."+name, sub)
			}
		}
	}
	for _, tool := range DefaultTools() {
		def := tool.Definition()
		check(def.Function.Name, "", def.Function.Parameters)
	}
}
