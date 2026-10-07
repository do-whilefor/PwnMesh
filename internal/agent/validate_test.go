package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateArgumentsEnumKeepsJSONTypesDistinct(t *testing.T) {
	values := []string{`null`, `true`, `false`, `"1"`, `1`, `[]`, `{}`}
	for _, allowed := range values {
		schema := json.RawMessage(`{"type":"object","properties":{"value":{"enum":[` + allowed + `]}}}`)
		for _, input := range values {
			t.Run(allowed+"/"+input, func(t *testing.T) {
				err := ValidateArguments(schema, json.RawMessage(`{"value":`+input+`}`))
				if (err == nil) != (input == allowed) {
					t.Fatalf("enum %s with input %s: %v", allowed, input, err)
				}
			})
		}
	}
}

func TestValidateArgumentsEnforcesEnumsInsideObjectsAndArrays(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"op":{"type":"string","enum":["curate"]},"payload":{"type":"object","properties":{"groups":{"type":"array","items":{"type":"object","properties":{"resolution":{"type":"string","enum":["resolved","uncertain"]},"sources":{"type":"array","items":{"type":"string","enum":["original","review"]}}},"required":["resolution"]}}}}},"required":["op","payload"]}`)
	for _, value := range []string{
		`{"op":"curate","payload":{"groups":[{"resolution":"resolved","sources":["original","review"]}]}}`,
		`{"op":"curate","payload":{"groups":[{"resolution":"uncertain"}]}}`,
	} {
		if err := ValidateArguments(schema, json.RawMessage(value)); err != nil {
			t.Fatalf("rejected declared enum value: %v", err)
		}
	}
	for _, tc := range []struct{ value, path string }{
		{`{"op":"step","payload":{"groups":[]}}`, "arguments.op"},
		{`{"op":"curate","payload":{"groups":[{"resolution":"closed"}]}}`, "arguments.payload.groups[0].resolution"},
		{`{"op":"curate","payload":{"groups":[{"resolution":"resolved","sources":["review","summary"]}]}}`, "arguments.payload.groups[0].sources[1]"},
	} {
		if err := ValidateArguments(schema, json.RawMessage(tc.value)); err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Fatalf("enum violation missing path %s: %v", tc.path, err)
		}
	}
}

func TestValidateArgumentsStructuredEnumsCompareValuesAndTypes(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"selection":{"type":"object","enum":[{"mode":"read","limits":[1,2],"enabled":true}]}}}`)
	if err := ValidateArguments(schema, json.RawMessage(`{"selection":{"enabled":true,"limits":[1.0,2],"mode":"read"}}`)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		`{"selection":{"mode":"write","limits":[1,2],"enabled":true}}`,
		`{"selection":{"mode":"read","limits":[2,1],"enabled":true}}`,
		`{"selection":{"mode":"read","limits":["1",2],"enabled":true}}`,
		`{"selection":{"mode":"read","limits":[1,2],"enabled":"true"}}`,
		`{"selection":{"mode":"read","limits":[1,2],"enabled":true,"extra":null}}`,
	} {
		if err := ValidateArguments(schema, json.RawMessage(value)); err == nil {
			t.Fatalf("accepted undeclared structured enum: %s", value)
		}
	}
	arraySchema := json.RawMessage(`{"type":"object","properties":{"order":{"type":"array","items":{"type":"string"},"enum":[["read","commit"]]}}}`)
	if err := ValidateArguments(arraySchema, json.RawMessage(`{"order":["read","commit"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateArguments(arraySchema, json.RawMessage(`{"order":["commit","read"]}`)); err == nil {
		t.Fatal("array enum ignored order")
	}
}

func TestNullableSchemaPreservesTypeAndValueConstraints(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"text":{"type":["string","null"]},"ids":{"type":["array","null"],"items":{"type":"string"}},"choice":{"type":["string","null"],"enum":["resolved","uncertain",null]},"required":{"type":"string"}},"required":["required"],"additionalProperties":false}`)
	for _, input := range []string{
		`{"required":"r"}`,
		`{"required":"r","text":null,"ids":null,"choice":null}`,
		`{"required":"r","text":"d","ids":["f"],"choice":"resolved"}`,
	} {
		if err := ValidateArguments(schema, json.RawMessage(input)); err != nil {
			t.Fatalf("valid nullable input %s: %v", input, err)
		}
	}
	for _, input := range []string{
		`{}`, `{"required":null}`, `{"required":"r","extra":null}`,
		`{"required":"r","text":false}`, `{"required":"r","text":[]}`,
		`{"required":"r","ids":"f"}`, `{"required":"r","ids":[null]}`,
		`{"required":"r","ids":["f",7]}`, `{"required":"r","choice":"closed"}`,
		`{"required":"r","choice":false}`,
	} {
		if err := ValidateArguments(schema, json.RawMessage(input)); err == nil {
			t.Fatalf("nullable schema accepted invalid input %s", input)
		}
	}
}

func TestSchemaRejectsUnknownTypesIncludingUnionBranches(t *testing.T) {
	for _, kind := range []string{`"unknown"`, `[]`, `["string","unknown"]`, `["string",null]`, `null`, `7`} {
		schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":` + kind + `}}}`)
		if err := ValidateArguments(schema, json.RawMessage(`{"value":"looks valid"}`)); err == nil {
			t.Fatalf("unsupported schema type %s was ignored", kind)
		}
	}
}
