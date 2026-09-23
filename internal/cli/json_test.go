// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tailscale.com/client/tailscale/v2"
)

func TestJSONValidationBeforeAPI(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"webhooks", "create", "--request", `{"endpointURL":"https://example.com"}`}, `$.endpointURL: unknown field; did you mean "endpointUrl"?`},
		{[]string{"webhooks", "create", "--request", `{"subscriptions":["known",4]}`}, `$.subscriptions[1]: expected string, got number`},
		{[]string{"keys", "create-auth-key", "--ckr", `{"capabilities":{"devices":{"create":{"reusable":null}}}}`}, `$.capabilities.devices.create.reusable: expected boolean, got null`},
		{[]string{"keys", "create-auth-key", "--ckr", `{"expirySeconds":1.5}`}, `$.expirySeconds: expected int64 integer`},
		{[]string{"keys", "create-auth-key", "--ckr", `{"expirySeconds":9223372036854775808}`}, `$.expirySeconds: expected int64 integer`},
		{[]string{"keys", "create-auth-key", "--ckr", `null`}, `$: expected object, got null`},
		{[]string{"keys", "create-auth-key", "--ckr", `{} {}`}, `$: expected a single JSON value`},
		{[]string{"keys", "create-auth-key", "--ckr", `{"capabilities":{"devices":{"create":{"reusable":null}}},"capabilities":{}}`}, `$.capabilities: duplicate field`},
		{[]string{"logging", "get-network-flow-logs", "--params", `{"Start":"yesterday"}`}, `$.Start: expected RFC3339 date-time`},
		{[]string{"devices", "set-posture-attribute", "--device-id", "d1", "--attribute-key", "custom:a", "--request", `{"expiry":5}`}, `$.expiry: expected string (sdk-time), got number`},
	} {
		t.Run(tt.want, func(t *testing.T) {
			cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %s", err, tt.want)
			}
		})
	}
}

func TestJSONSourcesUseSameValidation(t *testing.T) {
	const input = `{"email":42}`
	file := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(file, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{input, "@" + file, "@-"} {
		cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
		cmd.SetArgs([]string{"contacts", "update", "--contact-type", "security", "--contact", source})
		cmd.SetIn(strings.NewReader(input))
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--contact: $.email: expected string | null, got number") {
			t.Fatalf("%s: %v", source, err)
		}
	}
}

func TestJSONContracts(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		valid       bool
	}{
		{"CreateKeyRequest", `{}`, true},
		{"CreateKeyRequest", `{"expirySeconds":0,"description":"","capabilities":{"devices":{"create":{"reusable":false,"tags":null}}}}`, true},
		{"UpdateTailnetSettingsRequest", `{"devicesApprovalOn":null,"aclsExternallyManagedOn":false}`, true},
		{"Service", `{"annotations":{"example.com/key":"value"},"tags":null}`, true},
		{"Service", `{"annotations":{"example.com/key":42}}`, false},
		{"DevicePostureAttributeRequest", `{"value":{"arbitrary":[null,1,true]},"expiry":""}`, true},
		{"DevicePostureAttributeRequest", `{"value":null,"expiry":"2026-09-21T12:00:00Z"}`, true},
		{"DevicePostureAttributeRequest", `{"expiry":null}`, false},
		{"ACL", `{"ssh":[{"checkPeriod":"always"},{"checkPeriod":"1h30m"},{"checkPeriod":""}]}`, true},
		{"ACL", `{"ssh":[{"checkPeriod":"tomorrow"}]}`, false},
		{"ACL", `{"derpMap":{"regions":{"12":null}}}`, true},
		{"ACL", `{"derpMap":{"regions":{"hello":null}}}`, false},
		{"ACL", `{"ETag":"ignored previously"}`, false},
		{"CreateWebhookRequest", `{"providerType":"future-provider","subscriptions":["future-event"]}`, true},
	} {
		t.Run(tt.name+tt.input, func(t *testing.T) {
			err := inputSchemas.validate(inputSchemas.types[tt.name], []byte(tt.input))
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v, err=%v", tt.valid, err)
			}
		})
	}
	// A closed enum nested inside containers uses the same contract as string flags.
	shape := &jsonShape{Kind: "array", Elem: &jsonShape{Kind: "string", Enum: enumValues(tailscale.UserRole("")), Nullable: true}}
	if err := inputSchemas.validate(shape, []byte(`["admin",null]`)); err != nil {
		t.Fatal(err)
	}
	if err := inputSchemas.validate(shape, []byte(`["unknown"]`)); err == nil || !strings.Contains(err.Error(), "$[0]: must be one of") {
		t.Fatalf("%v", err)
	}
}

func TestJSONSchemaAndHelpOffline(t *testing.T) {
	for _, args := range [][]string{
		{"webhooks", "create", "schema", "--input", "request"},
		{"devices", "set-posture-attribute", "schema", "--input", "request"},
		{"policy-file", "set-and-get", "schema", "--input", "acl"},
	} {
		cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(out.Bytes(), &schema); err != nil {
			t.Fatal(err)
		}
		if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Fatalf("%s", out.String())
		}
		defs := schema["$defs"].(map[string]any)
		root := defs[strings.TrimPrefix(schema["$ref"].(string), "#/$defs/")].(map[string]any)
		if root["additionalProperties"] != false || root["required"] != nil {
			t.Fatalf("%v", root)
		}
		if strings.Contains(out.String(), `"ETag"`) {
			t.Fatal("json:- field included")
		}
	}
	cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"logging", "get-network-flow-logs", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"JSON inputs:", "--params <NetworkFlowLogsRequest>", "Start  string (date-time)", "Start must be set", "Show JSON Schema:\n    tailctl logging get-network-flow-logs schema --input params"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
	cmd = newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
	cmd.SetArgs([]string{"webhooks", "create", "schema", "--input", "invalid"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "schema: unknown JSON input") {
		t.Fatalf("%v", err)
	}
}

func TestRecursiveJSONContracts(t *testing.T) {
	inputSchemas.types["testNode"] = &jsonShape{Kind: "object", Fields: []jsonField{
		{Name: "children", Shape: &jsonShape{Kind: "map", Key: "string", Nullable: true, Elem: &jsonShape{Ref: "testNode", Nullable: true}}},
		{Name: "role", Shape: &jsonShape{Kind: "string", Enum: []string{"admin", "member"}}},
	}}
	t.Cleanup(func() { delete(inputSchemas.types, "testNode") })
	shape := &jsonShape{Ref: "testNode"}
	if err := inputSchemas.validate(shape, []byte(`{"children":{"one":{"children":{"two":null},"role":"member"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := inputSchemas.validate(shape, []byte(`{"children":{"example.com/key":{"role":"unknown"}}}`)); err == nil || !strings.Contains(err.Error(), `$.children["example.com/key"].role: must be one of`) {
		t.Fatalf("%v", err)
	}
	schema := inputSchemas.schemaDocument(shape)
	if len(schema["$defs"].(map[string]any)) != 1 {
		t.Fatalf("%v", schema)
	}
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	checkSchemaExamples(t, resolveExportedSchema(t, data),
		[]string{`{}`, `{"children":null}`, `{"children":{"one":{"children":{"two":null},"role":"member"}}}`},
		[]string{`{"children":{"one":{"role":"unknown"}}}`, `{"children":{"one":{"extra":true}}}`, `{"role":null}`})
	var help strings.Builder
	inputSchemas.help(shape, &help, "", map[string]bool{})
	if strings.Count(help.String(), "testNode:") != 1 {
		t.Fatal(help.String())
	}
}

func TestJSONSchemaConstraints(t *testing.T) {
	schema := inputSchemas.schemaDocument(inputSchemas.types["CreateKeyRequest"])
	props := schema["properties"].(map[string]any)
	expiry := props["expirySeconds"].(map[string]any)
	if expiry["minimum"] != int64(-9223372036854775808) || expiry["maximum"] != int64(9223372036854775807) {
		t.Fatalf("%v", expiry)
	}
	shape := &jsonShape{Kind: "string", Enum: []string{"admin"}, Nullable: true}
	schema = inputSchemas.schemaDocument(shape)
	variants := schema["anyOf"].([]any)
	if variants[1].(map[string]any)["type"] != "null" {
		t.Fatalf("%v", schema)
	}
	enum := variants[0].(map[string]any)["enum"].([]string)
	if len(enum) != 1 || enum[0] != "admin" {
		t.Fatalf("%v", schema)
	}
	shape = &jsonShape{Kind: "array", Fixed: true, Length: 2, Elem: &jsonShape{Kind: "integer", Integer: "uint8"}}
	for _, input := range []string{`[1]`, `[1,2,3]`, `[1,-1]`, `[1,256]`} {
		if err := inputSchemas.validate(shape, []byte(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	if err := inputSchemas.validate(shape, []byte(`[0,255]`)); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(inputSchemas.schemaDocument(shape))
	if err != nil {
		t.Fatal(err)
	}
	checkSchemaExamples(t, resolveExportedSchema(t, data),
		[]string{`[0,255]`},
		[]string{`[1]`, `[1,2,3]`, `[1,-1]`, `[1,256]`, `[1,1.5]`, `null`})
}

func TestNullableContracts(t *testing.T) {
	registry := schemaRegistry{types: map[string]*jsonShape{
		"Role": {Kind: "string", Enum: []string{"admin"}},
	}}
	for _, tt := range []struct {
		name    string
		shape   *jsonShape
		valid   string
		invalid string
	}{
		{"boolean", &jsonShape{Kind: "boolean", Nullable: true}, "true", "42"},
		{"enum", &jsonShape{Kind: "string", Enum: []string{"admin"}, Nullable: true}, `"admin"`, `"other"`},
		{"reference", &jsonShape{Ref: "Role", Nullable: true}, `"admin"`, `"other"`},
		{"sdk-time", &jsonShape{Kind: "string", Format: "sdk-time", Nullable: true}, `""`, "42"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, input := range []string{"null", tt.valid} {
				if err := registry.validate(tt.shape, []byte(input)); err != nil {
					t.Fatal(err)
				}
			}
			if err := registry.validate(tt.shape, []byte(tt.invalid)); err == nil {
				t.Fatalf("accepted %s", tt.invalid)
			}
			data, err := json.Marshal(registry.schemaDocument(tt.shape))
			if err != nil {
				t.Fatal(err)
			}
			checkSchemaExamples(t, resolveExportedSchema(t, data),
				[]string{"null", tt.valid}, []string{tt.invalid})
		})
	}
	if err := registry.validate(&jsonShape{Ref: "Role"}, []byte("null")); err == nil {
		t.Fatal("nullable reference changed the shared definition")
	}
}
