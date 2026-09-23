// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"tailscale.com/client/tailscale/v2"
)

func TestOutputHelp(t *testing.T) {
	for _, tt := range []struct {
		command string
		want    []string
		absent  []string
	}{
		{"devices list", []string{"Output:\n  JSON <(Device)[] | null>", "Device:", "id  string", "may be omitted", "string (date-time)"}, []string{"sdk-time"}},
		{"devices get", []string{"Output:\n  JSON <Device | null>"}, nil},
		{"dns nameservers", []string{"Output:\n  JSON <(string)[] | null>"}, nil},
		{"dns split-dns", []string{"Output:\n  JSON <SplitDNSResponse>"}, nil},
		{"logging get-network-flow-logs", []string{"JSON inputs:", "Output:\n  JSON Lines <NetworkFlowLog> (one value per line)", "TrafficStats:", "txBytes  integer (may be omitted)"}, nil},
		{"keys create-auth-key", []string{"JSON inputs:", "Output:\n  JSON <Key | null>", "integer (nanoseconds)"}, nil},
		{"policy-file raw", []string{"RawACL:", "HuJSON  string", "ETag  string"}, nil},
		{"devices delete", nil, []string{"Output:"}},
		{"contacts update", []string{"JSON inputs:"}, []string{"Output:"}},
		{"devices", nil, []string{"Output:"}},
		{"devices get schema", []string{"--output"}, []string{"--input"}},
		{"devices delete schema", []string{"--output"}, []string{"--input"}},
		{"webhooks create schema", []string{"--input", "--output", "values: request"}, nil},
	} {
		t.Run(tt.command, func(t *testing.T) {
			cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(append(strings.Fields(tt.command), "--help"))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			help := out.String()
			for _, want := range tt.want {
				if !strings.Contains(help, want) {
					t.Errorf("missing %q in help:\n%s", want, help)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(help, absent) {
					t.Errorf("unexpected %q in help", absent)
				}
			}
			previous := -1
			for _, section := range []string{"\nUsage:\n", "\nFlags:\n", "\nGlobal Flags:\n", "\nJSON inputs:\n", "\nOutput:\n"} {
				if start := strings.Index(help, section); start >= 0 {
					if start <= previous {
						t.Errorf("section %q is out of order", strings.TrimSpace(section))
					}
					previous = start
				}
			}
			if start := strings.Index(help, "Output:"); start >= 0 {
				if strings.Count(help[start:], "TrafficStats:") > 1 {
					t.Error("repeated output definition")
				}
			}
		})
	}
}

func TestOutputDefinitionsAreIndependentAndRecursive(t *testing.T) {
	node := &jsonShape{Ref: "Node"}
	defs := map[string]*jsonShape{
		"Node": {
			Kind: "object",
			Fields: []jsonField{
				{Name: "next", Shape: &jsonShape{Ref: "Node", Nullable: true}, MayOmit: true},
				{Name: "again", Shape: node},
				{Name: "created", Shape: &jsonShape{Ref: "Time"}},
			},
		},
		"Time": {Kind: "string", Format: "date-time"},
	}
	var out strings.Builder
	schemaRegistry{direction: encode, types: defs}.help(node, &out, "", map[string]bool{})
	help := out.String()
	if strings.Count(help, "Node:") != 1 || strings.Count(help, "Time:") != 1 {
		t.Fatalf("definitions must appear once: %s", help)
	}
	if !strings.Contains(help, "next  Node | null (may be omitted)") ||
		!strings.Contains(help, "string (date-time)") || strings.Contains(help, "sdk-time") {
		t.Fatalf("incorrect output representation: %s", help)
	}
}

func TestOutputSchemaOffline(t *testing.T) {
	for _, tt := range []struct {
		args      []string
		ref       string
		wantError string
	}{
		{[]string{"devices", "get", "schema", "--output"}, "Device", ""},
		{[]string{"webhooks", "create", "schema", "--input", "request", "--output"}, "", "none of the others can be"},
		{[]string{"webhooks", "create", "schema", "--input"}, "", "flag needs an argument: --input"},
		{[]string{"webhooks", "create", "schema", "--input="}, "", "unknown JSON input"},
		{[]string{"devices", "get", "schema", "response", "--output"}, "", "unknown command"},
		{[]string{"devices", "get"}, "", "required flag(s)"},
		{[]string{"devices", "get", "schema"}, "", "at least one of the flags in the group [output] is required"},
		{[]string{"webhooks", "create", "schema"}, "", "at least one of the flags in the group [input output] is required"},
		{[]string{"devices", "list", "schema", "--output"}, "Device", ""},
		{[]string{"keys", "create-auth-key", "schema", "--output"}, "Key", ""},
		{[]string{"logging", "get-network-flow-logs", "schema", "--output"}, "NetworkFlowLog", ""},
		{[]string{"devices", "delete", "schema", "--output"}, "", "command has no JSON output"},
		{[]string{"devices", "get", "schema", "--input", "invalid"}, "", "unknown flag: --input"},
		{[]string{"webhooks", "create", "schema", "--input", "request"}, "CreateWebhookRequest", ""},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetIn(failReader{t})
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v", err)
				}
				if out.Len() != 0 {
					t.Fatal("error wrote schema to stdout")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var schema map[string]any
			if err := json.Unmarshal(out.Bytes(), &schema); err != nil {
				t.Fatal(err)
			}
			if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
				t.Fatal(schema)
			}
			defs := schema["$defs"].(map[string]any)
			if defs[tt.ref] == nil {
				t.Fatalf("missing %s: %s", tt.ref, out.String())
			}
			if tt.ref == "NetworkFlowLog" && schema["$ref"] != "#/$defs/NetworkFlowLog" {
				t.Fatal("expected one log per line")
			}
			if tt.ref == "Device" {
				timestamp := defs["Time"].(map[string]any)
				if timestamp["format"] != "date-time" || timestamp["anyOf"] != nil {
					t.Fatal(timestamp)
				}
			}
		})
	}
}

type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) { r.t.Fatal("schema export read stdin"); return 0, nil }

func TestOutputSchemaRecursiveAndRequired(t *testing.T) {
	node := &jsonShape{Ref: "Node"}
	types := map[string]*jsonShape{"Node": {Kind: "object", Fields: []jsonField{
		{Name: "next", Shape: &jsonShape{Ref: "Node", Nullable: true}, MayOmit: true},
		{Name: "value", Shape: &jsonShape{Kind: "string"}},
		{Name: "bytes", Shape: &jsonShape{Kind: "string", Format: "base64", Nullable: true}},
	}}}
	schema := schemaRegistry{direction: encode, types: types}.schemaDocument(node)
	defs := schema["$defs"].(map[string]any)
	if len(defs) != 1 {
		t.Fatal(defs)
	}
	obj := defs["Node"].(map[string]any)
	if !reflect.DeepEqual(obj["required"], []string{"value", "bytes"}) {
		t.Fatal(obj)
	}
	props := obj["properties"].(map[string]any)
	if props["bytes"].(map[string]any)["contentEncoding"] != "base64" {
		t.Fatal(props)
	}
	variants := props["next"].(map[string]any)["anyOf"].([]any)
	if variants[0].(map[string]any)["$ref"] != "#/$defs/Node" {
		t.Fatal(variants)
	}
	input := schemaRegistry{direction: decode, types: types}.schemaDocument(node)
	if input["$defs"].(map[string]any)["Node"].(map[string]any)["required"] != nil {
		t.Fatal(input)
	}
}
