package artifactcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func validSpec(rules ...Rule) Spec {
	return Spec{Path: "/workspace/result.json", SHA256: strings.Repeat("0", 64), Rules: rules}
}

func TestEvaluateAllRulesAndIndependentHash(t *testing.T) {
	content := []byte(`{"status":"fixed","items":[null,{"ok":true}]}`)
	spec := validSpec(
		Rule{Kind: "json_valid"},
		Rule{Kind: "json_exists", Pointer: "/items/0"},
		Rule{Kind: "json_equals", Pointer: "/items/1/ok", Value: json.RawMessage(`true`)},
		Rule{Kind: "text_contains", Text: `"fixed"`},
		Rule{Kind: "text_not_contains", Text: "broken"},
	)
	result, err := Evaluate(spec, content)
	sum := sha256.Sum256(content)
	if err != nil || !result.Satisfied || len(result.FailedRules) != 0 || result.SHA256 != hex.EncodeToString(sum[:]) || result.SHA256 == spec.SHA256 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	spec.Rules[2].Value = json.RawMessage(`false`)
	spec.Rules[4].Text = "fixed"
	result, err = Evaluate(spec, content)
	if err != nil || result.Satisfied || !reflect.DeepEqual(result.FailedRules, []int{2, 4}) {
		t.Fatalf("failed rules: result=%+v error=%v", result, err)
	}
}

func TestJSONPointerMissingNullEmptyAndArrays(t *testing.T) {
	content := []byte(`{"":null,"a/b":{"~key":[null,"",{},[]]},"~1":true,"array":[3],"scalar":false}`)
	for _, tc := range []struct {
		pointer string
		exists  bool
	}{
		{"", true}, {"/", true}, {"/missing", false}, {"/missing/x", false},
		{"/a~1b/~0key/0", true}, {"/a~1b/~0key/1", true}, {"/a~1b/~0key/2", true}, {"/a~1b/~0key/3", true},
		{"/a~1b/~0key/4", false}, {"/a~1b/~0key/0/x", false}, {"/~01", true},
		{"/array/0", true}, {"/array/01", false}, {"/array/-", false}, {"/array/-1", false},
		{"/array/+0", false}, {"/array/", false}, {"/array/9999999999999999999999999999999", false}, {"/scalar/x", false},
	} {
		t.Run(tc.pointer, func(t *testing.T) {
			result, err := Evaluate(validSpec(Rule{Kind: "json_exists", Pointer: tc.pointer}), content)
			if err != nil || result.Satisfied != tc.exists {
				t.Fatalf("result=%+v error=%v want exists=%v", result, err, tc.exists)
			}
		})
	}
	for _, tc := range []struct{ pointer, expected string }{
		{"/", `null`}, {"/a~1b/~0key/0", `null`}, {"/a~1b/~0key/1", `""`},
		{"/a~1b/~0key/2", `{}`}, {"/a~1b/~0key/3", `[]`},
	} {
		result, err := Evaluate(validSpec(Rule{Kind: "json_equals", Pointer: tc.pointer, Value: json.RawMessage(tc.expected)}), content)
		if err != nil || !result.Satisfied {
			t.Fatalf("pointer=%q result=%+v error=%v", tc.pointer, result, err)
		}
	}
	for _, content := range []string{`null`, `""`, `{}`, `[]`} {
		result, err := Evaluate(validSpec(Rule{Kind: "json_exists"}), []byte(content))
		if err != nil || !result.Satisfied {
			t.Fatalf("root %s: result=%+v error=%v", content, result, err)
		}
	}
}

func TestJSONExactEquivalence(t *testing.T) {
	for _, tc := range []struct {
		actual, expected string
		equal            bool
	}{
		{`9007199254740993`, `9007199254740993`, true},
		{`9007199254740993`, `9007199254740992`, false},
		{`9007199254740993.0000001`, `9007199254740993.0000002`, false},
		{`9007199254740993000`, `9007199254740993e3`, true},
		{`1`, `1.0`, true}, {`1e+3`, `1000.000`, true}, {`-0.0e100`, `0`, true},
		{`1e-1000000000`, `10e-1000000001`, true},
		{`1e999999999999999999999999`, `10e999999999999999999999998`, true},
		{`1e-1000000000`, `0`, false}, {`-1`, `1`, false}, {`1`, `"1"`, false},
		{`true`, `1`, false}, {`false`, `null`, false}, {`"a"`, `"b"`, false},
		{`"\ud83d\ude00"`, `"😀"`, true}, {`"\\ud800"`, `"\\ud800"`, true},
		{`"\uFFFD"`, `"�"`, true},
		{`{"a":1,"b":[null,true]}`, `{"b":[null,true],"a":1.00}`, true},
		{`{"a":null}`, `{"b":null}`, false}, {`{"a":null}`, `{}`, false},
		{`[1,2]`, `[2,1]`, false}, {`[1]`, `[1,2]`, false}, {`{}`, `[]`, false},
	} {
		t.Run(tc.actual+"="+tc.expected, func(t *testing.T) {
			result, err := Evaluate(validSpec(Rule{Kind: "json_equals", Value: json.RawMessage(tc.expected)}), []byte(tc.actual))
			if err != nil || result.Satisfied != tc.equal {
				t.Fatalf("result=%+v error=%v want equal=%v", result, err, tc.equal)
			}
		})
	}
}

func TestInvalidJSONFailsRules(t *testing.T) {
	for _, content := range []string{
		``, ` `, `{`, `{"a":1,"a":2}`, `{"a":{"x":1,"x":1}}`, `{"a":1,"\u0061":2}`,
		`true false`, `{}x`, `[1,]`, `NaN`, `01`, `"unterminated`,
		`"\ud800"`, `"\udfff"`, `"\ud800\ud800"`, `"\ud800\uzzzz"`,
		`"\ud800\u0000"`, `"\ud800\\udc00"`, `"\u12"`, `"\uzzzz"`,
		strings.Repeat("[", MaxJSONDepth+1) + "0" + strings.Repeat("]", MaxJSONDepth+1),
		"1" + strings.Repeat("0", maxNumberBytes),
	} {
		t.Run(content[:min(len(content), 40)], func(t *testing.T) {
			result, err := Evaluate(validSpec(
				Rule{Kind: "json_valid"}, Rule{Kind: "json_exists"},
				Rule{Kind: "json_equals", Value: json.RawMessage(`null`)},
				Rule{Kind: "text_not_contains", Text: "impossible"},
			), []byte(content))
			if err != nil || result.Satisfied || !reflect.DeepEqual(result.FailedRules, []int{0, 1, 2}) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestValidateRejectsInvalidSpecs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Spec)
	}{
		{"relative", func(s *Spec) { s.Path = "result.json" }},
		{"root", func(s *Spec) { s.Path = "/" }},
		{"traversal", func(s *Spec) { s.Path = "/workspace/../result.json" }},
		{"double slash", func(s *Spec) { s.Path = "/workspace//result.json" }},
		{"trailing slash", func(s *Spec) { s.Path = "/workspace/" }},
		{"NUL", func(s *Spec) { s.Path = "/workspace/\x00" }},
		{"backslash", func(s *Spec) { s.Path = `/workspace\result` }},
		{"long path", func(s *Spec) { s.Path = "/" + strings.Repeat("a", MaxPathBytes) }},
		{"invalid path UTF8", func(s *Spec) { s.Path = "/\xff" }},
		{"short hash", func(s *Spec) { s.SHA256 = "123" }},
		{"uppercase hash", func(s *Spec) { s.SHA256 = strings.Repeat("A", 64) }},
		{"nonhex hash", func(s *Spec) { s.SHA256 = strings.Repeat("z", 64) }},
		{"missing rules", func(s *Spec) { s.Rules = nil }},
		{"excess rules", func(s *Spec) { s.Rules = make([]Rule, MaxRules+1) }},
		{"unknown kind", func(s *Spec) { s.Rules[0].Kind = "shell" }},
		{"valid pointer", func(s *Spec) { s.Rules[0].Pointer = "/a" }},
		{"valid value", func(s *Spec) { s.Rules[0].Value = json.RawMessage(`null`) }},
		{"valid text", func(s *Spec) { s.Rules[0].Text = "a" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec(Rule{Kind: "json_valid"})
			tc.change(&spec)
			if err := Validate(spec); err == nil {
				t.Fatalf("accepted invalid spec: %+v", spec)
			}
			if _, err := Evaluate(spec, []byte(`null`)); err == nil {
				t.Fatal("Evaluate accepted invalid spec")
			}
		})
	}
	for _, rule := range []Rule{
		{Kind: "json_exists", Pointer: "a"}, {Kind: "json_exists", Pointer: "/a~"},
		{Kind: "json_exists", Pointer: "/a~2"}, {Kind: "json_exists", Pointer: "/\xff"},
		{Kind: "json_exists", Pointer: "/" + strings.Repeat("a", MaxPathBytes)},
		{Kind: "json_exists", Value: json.RawMessage(`null`)}, {Kind: "json_exists", Text: "x"},
		{Kind: "json_equals"}, {Kind: "json_equals", Value: json.RawMessage(`1 2`)},
		{Kind: "json_equals", Value: json.RawMessage(`{"a":1,"a":2}`)},
		{Kind: "json_equals", Value: json.RawMessage(`"` + strings.Repeat("a", MaxValueBytes) + `"`)},
		{Kind: "text_contains"}, {Kind: "text_not_contains", Text: "\xff"},
		{Kind: "text_contains", Text: strings.Repeat("界", MaxTextBytes/3+1)},
		{Kind: "text_contains", Text: "x", Pointer: "/a"},
		{Kind: "text_contains", Text: "x", Value: json.RawMessage(`null`)},
	} {
		if err := Validate(validSpec(rule)); err == nil {
			t.Errorf("accepted invalid rule: %.100v", rule)
		}
	}
}

func TestStrictJSONDecoding(t *testing.T) {
	for _, data := range []string{
		`null`, `[]`, `{"unknown":1}`, `{"Path":"/a"}`, `{"path":"/a","path":"/b"}`,
		`{"rules":[null]}`, `{"rules":[{"kind":"json_valid","unknown":true}]}`,
		`{"rules":[{"kind":"json_valid","Kind":"text_contains"}]}`,
		`{"rules":[{"kind":"json_valid","kind":"json_exists"}]}`,
		`{"rules":[{"kind":null}]}`, `{"rules":[{"kind":"json_exists","pointer":null}]}`,
		`{"rules":[{"kind":"text_contains","text":null}]}`, `{} {}`,
	} {
		var spec Spec
		if err := json.Unmarshal([]byte(data), &spec); err == nil {
			t.Errorf("decoded invalid spec: %s", data)
		}
	}
	for _, data := range []string{
		`{"kind":"json_valid","pointer":""}`, `{"kind":"json_valid","text":""}`,
		`{"kind":"json_exists","text":""}`, `{"kind":"text_contains","text":"x","pointer":""}`,
	} {
		var rule Rule
		if err := json.Unmarshal([]byte(data), &rule); err != nil {
			t.Fatal(err)
		}
		if err := Validate(validSpec(rule)); err == nil {
			t.Errorf("accepted incompatible empty field: %s", data)
		}
	}
	var rule Rule
	if err := json.Unmarshal([]byte(`{"kind":"json_equals","pointer":"","value":null}`), &rule); err != nil {
		t.Fatal(err)
	}
	result, err := Evaluate(validSpec(rule), []byte(`null`))
	if err != nil || !result.Satisfied {
		t.Fatalf("explicit null value/root pointer: %+v, %v", result, err)
	}
	encoded, err := json.Marshal(validSpec(rule))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Spec
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := Validate(decoded); err != nil {
		t.Fatal(err)
	}
}

func TestResourceLimits(t *testing.T) {
	spec := validSpec(Rule{Kind: "text_contains", Text: "a"})
	for _, size := range []int{MaxFileBytes, MaxFileBytes + 1} {
		result, err := Evaluate(spec, []byte(strings.Repeat("a", size)))
		if size == MaxFileBytes && (err != nil || !result.Satisfied) {
			t.Fatalf("maximum file rejected: %+v, %v", result, err)
		}
		if size > MaxFileBytes && err == nil {
			t.Fatal("oversized file accepted")
		}
	}
	if _, err := Evaluate(spec, []byte{0xff}); err == nil {
		t.Fatal("non-UTF8 file accepted")
	}
	for _, content := range []string{
		strings.Repeat("[", MaxJSONDepth) + "0" + strings.Repeat("]", MaxJSONDepth),
		"1" + strings.Repeat("0", maxNumberBytes-1),
	} {
		result, err := Evaluate(validSpec(Rule{Kind: "json_valid"}), []byte(content))
		if err != nil || !result.Satisfied {
			t.Fatalf("boundary JSON rejected: %+v, %v", result, err)
		}
		// Contract wrappers do not consume the expected value's depth budget.
		encoded, err := json.Marshal(validSpec(Rule{Kind: "json_equals", Value: json.RawMessage(content)}))
		if err != nil {
			t.Fatal(err)
		}
		var decoded Spec
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("boundary JSON value failed round trip: %v", err)
		}
		if err := Validate(decoded); err != nil {
			t.Fatal(err)
		}
	}
	spec.Path = "/" + strings.Repeat("a", MaxPathBytes-1)
	spec.Rules = []Rule{{Kind: "text_contains", Text: strings.Repeat("a", MaxTextBytes)}}
	if err := Validate(spec); err != nil {
		t.Fatalf("boundary text/path rejected: %v", err)
	}
	spec.Rules = []Rule{{Kind: "json_equals", Value: json.RawMessage(`"` + strings.Repeat("a", MaxValueBytes-2) + `"`)}}
	if err := Validate(spec); err != nil {
		t.Fatalf("boundary value rejected: %v", err)
	}
	spec.Rules = make([]Rule, MaxRules)
	for i := range spec.Rules {
		spec.Rules[i] = Rule{Kind: "json_exists", Pointer: "/" + strings.Repeat("a", MaxPathBytes-1)}
	}
	if err := Validate(spec); err != nil {
		t.Fatalf("boundary rule count/pointer rejected: %v", err)
	}
}

func TestSchemaIsJSONSerializable(t *testing.T) {
	schema := Schema()
	if _, err := json.Marshal(schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false || !reflect.DeepEqual(schema["required"], []string{"path", "sha256", "rules"}) {
		t.Fatalf("incomplete spec schema: %v", schema)
	}
}

func FuzzJSONEvaluation(f *testing.F) {
	for _, seed := range []string{`null`, `{"x":1}`, `{"x":1,"x":2}`, `[null,{}]`, `1e10000000000`, `{} false`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, content string) {
		if len(content) > MaxFileBytes {
			t.Skip()
		}
		_, _ = Evaluate(validSpec(Rule{Kind: "json_valid"}, Rule{Kind: "json_exists", Pointer: "/x"}), []byte(content))
	})
}
