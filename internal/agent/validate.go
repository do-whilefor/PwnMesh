package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// ValidateArguments supports the object/primitive/array schema vocabulary used
// by the seven built-in tools. Tools retain their own semantic validation.
func ValidateArguments(schema, input json.RawMessage) error {
	var value any
	d := json.NewDecoder(bytes.NewReader(input))
	d.UseNumber()
	if !json.Valid(input) {
		return errors.New("tool arguments must be valid JSON")
	}
	if err := d.Decode(&value); err != nil {
		return err
	}
	if _, ok := value.(map[string]any); !ok {
		return errors.New("tool arguments must be an object")
	}
	if len(schema) == 0 {
		return nil
	}
	var spec map[string]any
	if err := json.Unmarshal(schema, &spec); err != nil {
		return fmt.Errorf("invalid tool schema: %w", err)
	}
	return validateValue(spec, value, "arguments")
}

func validateValue(s map[string]any, v any, path string) error {
	fail := func() error { return fmt.Errorf("%s must be %s", path, s["type"]) }
	if types, ok := s["type"].([]any); ok {
		if len(types) == 0 {
			return fmt.Errorf("%s has an empty schema type union", path)
		}
		for _, kind := range types {
			if name, ok := kind.(string); !ok || !schemaType(name) {
				return fmt.Errorf("%s has an unsupported schema type", path)
			}
		}
		branch := make(map[string]any, len(s))
		for key, value := range s {
			branch[key] = value
		}
		for _, kind := range types {
			branch["type"] = kind
			if validateValue(branch, v, path) == nil {
				return nil
			}
		}
		return fail()
	}
	switch s["type"] {
	case nil: // Constraints such as enum may appear without a type.
		if _, specified := s["type"]; specified {
			return fmt.Errorf("%s has an unsupported schema type", path)
		}
	case "null":
		if v != nil {
			return fail()
		}
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fail()
		}
		props, _ := s["properties"].(map[string]any)
		if required, ok := s["required"].([]any); ok {
			for _, key := range required {
				k, _ := key.(string)
				if _, ok := obj[k]; !ok {
					return fmt.Errorf("missing argument %s.%s", path, k)
				}
			}
		}
		for k, value := range obj {
			prop, exists := props[k]
			if !exists {
				if s["additionalProperties"] == false {
					return fmt.Errorf("unknown argument %s.%s", path, k)
				}
				continue
			}
			if def, ok := prop.(map[string]any); ok {
				if err := validateValue(def, value, path+"."+k); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fail()
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fail()
		}
	case "integer", "number":
		n, ok := v.(json.Number)
		if !ok {
			return fail()
		}
		f, err := n.Float64()
		if err != nil || math.IsInf(f, 0) {
			return fail()
		}
		if s["type"] == "integer" && math.Trunc(f) != f {
			return fail()
		}
		if min, ok := s["minimum"].(float64); ok && f < min {
			return fmt.Errorf("%s must be at least %g", path, min)
		}
		if max, ok := s["maximum"].(float64); ok && f > max {
			return fmt.Errorf("%s must be at most %g", path, max)
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return fail()
		}
		if item, ok := s["items"].(map[string]any); ok {
			for i, value := range a {
				if err := validateValue(item, value, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("%s has an unsupported schema type", path)
	}
	if allowed, ok := s["enum"].([]any); ok {
		for _, candidate := range allowed {
			if enumEqual(candidate, v) {
				return nil
			}
		}
		return fmt.Errorf("%s must match one of its allowed enum values", path)
	}
	return nil
}

func schemaType(kind string) bool {
	switch kind {
	case "object", "string", "boolean", "integer", "number", "array", "null":
		return true
	}
	return false
}

// Schema numbers are float64; input numbers retain json.Number so type and
// integer checks can run first. Compare recursively for object/array enums.
func enumEqual(expected, actual any) bool {
	switch value := actual.(type) {
	case json.Number:
		number, ok := expected.(float64)
		if !ok {
			return false
		}
		parsed, err := value.Float64()
		return err == nil && !math.IsInf(parsed, 0) && parsed == number
	case []any:
		other, ok := expected.([]any)
		if !ok || len(other) != len(value) {
			return false
		}
		for i := range value {
			if !enumEqual(other[i], value[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		other, ok := expected.(map[string]any)
		if !ok || len(other) != len(value) {
			return false
		}
		for key, entry := range value {
			item, exists := other[key]
			if !exists || !enumEqual(item, entry) {
				return false
			}
		}
		return true
	default:
		// The remaining decoded JSON values are comparable primitives.
		return expected == actual
	}
}
