package tools

import "testing"

// TestToolSchemasAreStrictCompatible checks every built-in tool schema against the constraints of
// strict tool use: a closed object (additionalProperties false), a required list present, and only
// declared properties named as required.
func TestToolSchemasAreStrictCompatible(t *testing.T) {
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
