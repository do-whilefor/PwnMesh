// Package artifactcheck evaluates bounded, deterministic artifact repair rules.
package artifactcheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxFileBytes   = 4 << 20
	MaxRules       = 16
	MaxPathBytes   = 4096
	MaxTextBytes   = 4096
	MaxValueBytes  = 64 << 10
	MaxJSONDepth   = 64
	maxNumberBytes = 1024
)

type Spec struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Rules  []Rule `json:"rules"`
}

type Rule struct {
	Kind                string          `json:"kind"`
	Pointer             string          `json:"pointer,omitempty"`
	Value               json.RawMessage `json:"value,omitempty"`
	Text                string          `json:"text,omitempty"`
	pointerSet, textSet bool
}

type Result struct {
	SHA256      string `json:"sha256"`
	Satisfied   bool   `json:"satisfied"`
	FailedRules []int  `json:"failed_rules"` // Zero-based indexes.
}

// UnmarshalJSON rejects unknown fields, duplicate keys, and non-object specs.
func (s *Spec) UnmarshalJSON(data []byte) error {
	type plain Spec
	var decoded plain
	if _, err := unmarshalObject(data, &decoded, "path", "sha256", "rules"); err != nil {
		return err
	}
	*s = Spec(decoded)
	return nil
}

func (r *Rule) UnmarshalJSON(data []byte) error {
	type plain Rule
	var decoded plain
	fields, err := unmarshalObject(data, &decoded, "kind", "pointer", "value", "text")
	if err != nil {
		return err
	}
	_, decoded.pointerSet = fields["pointer"]
	_, decoded.textSet = fields["text"]
	// encoding/json normally accepts null for strings; rule fields must be typed.
	for _, name := range []string{"kind", "pointer", "text"} {
		if value, present := fields[name]; present {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s must be a string", name)
			}
		}
	}
	*r = Rule(decoded)
	return nil
}

func unmarshalObject(data []byte, dst any, allowed ...string) (map[string]any, error) {
	// A spec wraps rule values in an object, rules array, and rule object.
	value, err := parseJSONDepth(data, MaxJSONDepth+3)
	if err != nil {
		return nil, err
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected an object")
	}
	for name := range fields {
		known := false
		for _, field := range allowed {
			known = known || name == field
		}
		if !known {
			return nil, fmt.Errorf("unknown field %q", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return nil, err
	}
	return fields, nil
}

// Validate checks the portable contract. The caller must additionally confine
// Path to its actual workspace and enforce its filesystem access policy.
func Validate(spec Spec) error {
	if len(spec.Path) > MaxPathBytes || !utf8.ValidString(spec.Path) ||
		!path.IsAbs(spec.Path) || spec.Path == "/" || path.Clean(spec.Path) != spec.Path ||
		strings.ContainsAny(spec.Path, "\x00\\") {
		return fmt.Errorf("path must be a canonical absolute Linux file path of at most %d bytes", MaxPathBytes)
	}
	if len(spec.SHA256) != 64 || strings.Trim(spec.SHA256, "0123456789abcdef") != "" {
		return fmt.Errorf("sha256 must contain 64 lowercase hexadecimal characters")
	}
	if len(spec.Rules) == 0 || len(spec.Rules) > MaxRules {
		return fmt.Errorf("rules must contain 1 to %d entries", MaxRules)
	}
	for i, rule := range spec.Rules {
		if err := validateRule(rule); err != nil {
			return fmt.Errorf("rule %d: %w", i, err)
		}
	}
	return nil
}

func validateRule(rule Rule) error {
	hasPointer := rule.pointerSet || rule.Pointer != ""
	hasText := rule.textSet || rule.Text != ""
	hasValue := len(rule.Value) != 0
	switch rule.Kind {
	case "json_valid":
		if hasPointer || hasText || hasValue {
			return fmt.Errorf("json_valid accepts only kind")
		}
	case "json_exists", "json_equals":
		if hasText || (rule.Kind == "json_exists" && hasValue) {
			return fmt.Errorf("incompatible field for %s", rule.Kind)
		}
		if _, err := pointerTokens(rule.Pointer); err != nil {
			return err
		}
		if rule.Kind == "json_equals" {
			if !hasValue || len(rule.Value) > MaxValueBytes {
				return fmt.Errorf("value must be JSON of at most %d bytes", MaxValueBytes)
			}
			if _, err := parseJSON(rule.Value); err != nil {
				return fmt.Errorf("invalid value: %w", err)
			}
		}
	case "text_contains", "text_not_contains":
		if hasPointer || hasValue {
			return fmt.Errorf("incompatible field for %s", rule.Kind)
		}
		if rule.Text == "" || len(rule.Text) > MaxTextBytes || !utf8.ValidString(rule.Text) {
			return fmt.Errorf("text must be nonempty UTF-8 of at most %d bytes", MaxTextBytes)
		}
	default:
		return fmt.Errorf("unsupported kind %q", rule.Kind)
	}
	return nil
}

// Evaluate returns the actual content hash independently of the bound hash in
// spec. Satisfied means all rules passed, even if the bound hash is stale.
// Malformed JSON fails JSON rules; invalid UTF-8 or oversized files return an
// error. All rules use literal matching, with no scripts or regular expressions.
func Evaluate(spec Spec, content []byte) (Result, error) {
	if err := Validate(spec); err != nil {
		return Result{}, err
	}
	if len(content) > MaxFileBytes {
		return Result{}, fmt.Errorf("file exceeds %d bytes", MaxFileBytes)
	}
	if !utf8.Valid(content) {
		return Result{}, fmt.Errorf("file must be UTF-8")
	}
	sum := sha256.Sum256(content)
	result := Result{SHA256: hex.EncodeToString(sum[:]), Satisfied: true, FailedRules: []int{}}
	var document any
	var jsonErr error
	for _, rule := range spec.Rules {
		if strings.HasPrefix(rule.Kind, "json_") {
			document, jsonErr = parseJSON(content)
			break
		}
	}
	for i, rule := range spec.Rules {
		passed := false
		switch rule.Kind {
		case "json_valid":
			passed = jsonErr == nil
		case "json_exists", "json_equals":
			if jsonErr == nil {
				tokens, _ := pointerTokens(rule.Pointer)
				value, exists := lookup(document, tokens)
				passed = exists
				if passed && rule.Kind == "json_equals" {
					expected, _ := parseJSON(rule.Value) // Validated above.
					passed = equalJSON(value, expected)
				}
			}
		case "text_contains":
			passed = bytes.Contains(content, []byte(rule.Text))
		case "text_not_contains":
			passed = !bytes.Contains(content, []byte(rule.Text))
		}
		if !passed {
			result.Satisfied = false
			result.FailedRules = append(result.FailedRules, i)
		}
	}
	return result, nil
}

func pointerTokens(pointer string) ([]string, error) {
	if len(pointer) > MaxPathBytes || !utf8.ValidString(pointer) || (pointer != "" && pointer[0] != '/') {
		return nil, fmt.Errorf("pointer must be an RFC 6901 pointer of at most %d bytes", MaxPathBytes)
	}
	if pointer == "" {
		return nil, nil
	}
	tokens := strings.Split(pointer[1:], "/")
	for i, token := range tokens {
		for j := 0; j < len(token); j++ {
			if token[j] == '~' {
				j++
				if j == len(token) || (token[j] != '0' && token[j] != '1') {
					return nil, fmt.Errorf("invalid RFC 6901 escape")
				}
			}
		}
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
	}
	return tokens, nil
}

func lookup(value any, tokens []string) (any, bool) {
	for _, token := range tokens {
		switch container := value.(type) {
		case map[string]any:
			var exists bool
			value, exists = container[token]
			if !exists {
				return nil, false
			}
		case []any:
			if token == "" || strings.Trim(token, "0123456789") != "" || (len(token) > 1 && token[0] == '0') {
				return nil, false
			}
			index, err := strconv.Atoi(token)
			if err != nil || index >= len(container) {
				return nil, false
			}
			value = container[index]
		default:
			return nil, false
		}
	}
	return value, true
}

func parseJSON(data []byte) (any, error) {
	return parseJSONDepth(data, MaxJSONDepth)
}

func parseJSONDepth(data []byte, maxDepth int) (any, error) {
	if !utf8.Valid(data) || !validUnicodeEscapes(data) {
		return nil, fmt.Errorf("JSON must contain valid Unicode")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readJSON(decoder, 0, maxDepth)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	return value, nil
}

// encoding/json replaces unpaired surrogate escapes with U+FFFD. Reject them
// before decoding so distinct malformed strings cannot compare equal.
func validUnicodeEscapes(data []byte) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		i++ // Skip escaped backslashes, which do not introduce Unicode escapes.
		if i >= len(data) || data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		code, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil || (code >= 0xdc00 && code <= 0xdfff) {
			return false
		}
		i += 4
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

func readJSON(decoder *json.Decoder, depth, maxDepth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("JSON exceeds maximum depth %d", maxDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key := keyToken.(string)
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate JSON key")
			}
			value, err := readJSON(decoder, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		_, err = decoder.Token()
		return object, err
	case json.Delim('['):
		array := []any{}
		for decoder.More() {
			value, err := readJSON(decoder, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err = decoder.Token()
		return array, err
	default:
		if number, ok := token.(json.Number); ok && len(number) > maxNumberBytes {
			return nil, fmt.Errorf("JSON number exceeds %d bytes", maxNumberBytes)
		}
		return token, nil
	}
}

func equalJSON(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case bool:
		other, ok := b.(bool)
		return ok && a == other
	case string:
		other, ok := b.(string)
		return ok && a == other
	case json.Number:
		other, ok := b.(json.Number)
		return ok && canonicalNumber(a) == canonicalNumber(other)
	case []any:
		other, ok := b.([]any)
		if !ok || len(a) != len(other) {
			return false
		}
		for i := range a {
			if !equalJSON(a[i], other[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		other, ok := b.(map[string]any)
		if !ok || len(a) != len(other) {
			return false
		}
		for key, value := range a {
			expected, exists := other[key]
			if !exists || !equalJSON(value, expected) {
				return false
			}
		}
		return true
	}
	return false
}

// Canonicalize decimal coefficients and exponents without expanding exponents
// or rounding numbers through float64. Input is already validated JSON.
func canonicalNumber(number json.Number) string {
	value := string(number)
	sign := ""
	if value[0] == '-' {
		sign, value = "-", value[1:]
	}
	exponent := new(big.Int)
	if i := strings.IndexAny(value, "eE"); i >= 0 {
		exponent.SetString(value[i+1:], 10)
		value = value[:i]
	}
	if i := strings.IndexByte(value, '.'); i >= 0 {
		exponent.Sub(exponent, big.NewInt(int64(len(value)-i-1)))
		value = value[:i] + value[i+1:]
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0"
	}
	coefficient := strings.TrimRight(value, "0")
	exponent.Add(exponent, big.NewInt(int64(len(value)-len(coefficient))))
	return sign + coefficient + "e" + exponent.String()
}

// Schema describes the same limited rule language for model tool inputs.
func Schema() map[string]any {
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	variants := []any{}
	for _, kind := range []string{"json_valid", "json_exists", "json_equals", "text_contains", "text_not_contains"} {
		properties := map[string]any{"kind": map[string]any{"type": "string", "enum": []string{kind}}}
		required := []string{"kind"}
		if kind == "json_exists" || kind == "json_equals" {
			properties["pointer"] = map[string]any{"type": "string", "maxLength": MaxPathBytes, "description": "RFC 6901 pointer; empty or omitted selects the root"}
		}
		if kind == "json_equals" {
			properties["value"] = map[string]any{"description": "Expected JSON value; at most 64 KiB encoded, exact decimal number comparison"}
			required = append(required, "value")
		}
		if strings.HasPrefix(kind, "text_") {
			properties["text"] = map[string]any{"type": "string", "minLength": 1, "maxLength": MaxTextBytes}
			required = append(required, "text")
		}
		variants = append(variants, object(properties, required...))
	}
	return object(map[string]any{
		"path":   map[string]any{"type": "string", "maxLength": MaxPathBytes, "description": "Canonical absolute Linux workspace file path"},
		"sha256": map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$", "description": "SHA-256 of the artifact version that needs repair"},
		"rules":  map[string]any{"type": "array", "minItems": 1, "maxItems": MaxRules, "items": map[string]any{"oneOf": variants}},
	}, "path", "sha256", "rules")
}
