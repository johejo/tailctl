// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"tailscale.com/client/tailscale/v2"
)

func resolveExportedSchema(t *testing.T, data []byte) *jsonschema.Resolved {
	t.Helper()
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func checkSchemaExamples(t *testing.T, schema *jsonschema.Resolved, valid, invalid []string) {
	t.Helper()
	// These examples exercise JSON Schema constraints, not SDK decoding rules
	// such as duplicate keys, integer notation, or custom duration formats.
	// Keep numbers exactly representable as float64; Go integer boundaries are
	// covered separately by the CLI validation and schema export tests.
	for _, group := range []struct {
		inputs []string
		valid  bool
	}{{valid, true}, {invalid, false}} {
		for _, input := range group.inputs {
			t.Run(input, func(t *testing.T) {
				var value any
				if err := json.Unmarshal([]byte(input), &value); err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(value); (err == nil) != group.valid {
					t.Fatalf("valid=%v, err=%v", group.valid, err)
				}
			})
		}
	}
}

func TestGeneratedSchemasResolve(t *testing.T) {
	for name := range inputSchemas.types {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(inputSchemas.schemaDocument(&jsonShape{Ref: name}))
			if err != nil {
				t.Fatal(err)
			}
			resolveExportedSchema(t, data)
		})
	}
}

func TestExportedSchemaValidation(t *testing.T) {
	for _, tt := range []struct {
		name           string
		args           []string
		valid, invalid []string
	}{
		{
			name:    "webhook",
			args:    []string{"webhooks", "create", "schema", "--input", "request"},
			valid:   []string{`{}`, `{"endpointUrl":"https://example.com","subscriptions":["future-event"]}`, `{"subscriptions":null}`},
			invalid: []string{`null`, `{"endpointURL":"https://example.com"}`, `{"subscriptions":[4]}`, `{"endpointUrl":null}`},
		},
		{
			name:    "key",
			args:    []string{"keys", "create-auth-key", "schema", "--input", "ckr"},
			valid:   []string{`{}`, `{"expirySeconds":3600,"capabilities":{"devices":{"create":{"reusable":false,"tags":["tag:test"]}}}}`},
			invalid: []string{`{"expirySeconds":1.5}`, `{"expirySeconds":"3600"}`, `{"capabilities":{"devices":{"create":{"reusable":null}}}}`, `{"capabilities":{"unknown":true}}`},
		},
		{
			name:    "posture",
			args:    []string{"devices", "set-posture-attribute", "schema", "--input", "request"},
			valid:   []string{`{}`, `{"value":{"arbitrary":[null,1,true]},"expiry":""}`, `{"value":null,"expiry":"2026-09-21T12:00:00Z"}`},
			invalid: []string{`{"expiry":5}`, `{"expiry":null}`, `{"unknown":true}`},
		},
		{
			name:    "policy",
			args:    []string{"policy-file", "set-and-get", "schema", "--input", "acl"},
			valid:   []string{`{}`, `{"derpMap":{"regions":{"12":null}}}`},
			invalid: []string{`{"ETag":"hidden"}`, `{"derpMap":{"regions":{"hello":null}}}`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			checkSchemaExamples(t, resolveExportedSchema(t, out.Bytes()), tt.valid, tt.invalid)
		})
	}
}
