package chat

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"slices"
)

// PropertyOrder is the key under which a schema object lists its properties in the order they are
// sent, so the model reads each tool's parameters in a sensible order (path before the optional
// ones) rather than sorted. The key itself is never sent.
const PropertyOrder = "propertyOrder"

// Schema is a tool's JSON Schema. It is sent with its keys sorted, so the prompt the API caches
// stays the same from run to run, except that each object's properties come in its PropertyOrder,
// then any it leaves out, sorted.
type Schema map[string]any

// MarshalJSONTo writes the schema with each object's properties in their PropertyOrder.
func (s Schema) MarshalJSONTo(enc *jsontext.Encoder) error {
	return writeSchemaValue(enc, map[string]any(s))
}

func writeSchemaValue(enc *jsontext.Encoder, v any) error {
	switch v := v.(type) {
	case Schema:
		return writeSchemaValue(enc, map[string]any(v))
	case map[string]any:
		if err := enc.WriteToken(jsontext.BeginObject); err != nil {
			return err
		}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if k == PropertyOrder {
				continue
			}
			if err := enc.WriteToken(jsontext.String(k)); err != nil {
				return err
			}
			var err error
			if props, ok := v[k].(map[string]any); ok && k == "properties" {
				order, _ := v[PropertyOrder].([]string)
				err = writeProperties(enc, props, order)
			} else {
				err = writeSchemaValue(enc, v[k])
			}
			if err != nil {
				return err
			}
		}
		return enc.WriteToken(jsontext.EndObject)
	case []any:
		if err := enc.WriteToken(jsontext.BeginArray); err != nil {
			return err
		}
		for _, e := range v {
			if err := writeSchemaValue(enc, e); err != nil {
				return err
			}
		}
		return enc.WriteToken(jsontext.EndArray)
	default:
		return json.MarshalEncode(enc, v, json.Deterministic(true))
	}
}

// writeProperties writes a properties object with the names in order first, then the rest sorted.
func writeProperties(enc *jsontext.Encoder, props map[string]any, order []string) error {
	var names []string
	for _, name := range order {
		if _, ok := props[name]; ok && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(props)) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, name := range names {
		if err := enc.WriteToken(jsontext.String(name)); err != nil {
			return err
		}
		if err := writeSchemaValue(enc, props[name]); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}
