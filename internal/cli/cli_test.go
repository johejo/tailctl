// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"tailscale.com/client/tailscale/v2"
)

func TestGeneratedCommands(t *testing.T) {
	tests := []struct {
		name                                                            string
		args                                                            []string
		stdin, method, path, query, body, response, output, contentType string
	}{
		{name: "grouped string arguments", args: []string{"devices", "set-name", "--device-id", "d1", "--name", "new"}, method: "POST", path: "/api/v2/device/d1/name", body: `{"name":"new"}`},
		{name: "explicit false", args: []string{"devices", "set-authorized", "--device-id", "d1", "--authorized=false"}, method: "POST", path: "/api/v2/device/d1/authorized", body: `{"authorized":false}`},
		{name: "JSON stdin", args: []string{"contacts", "update", "--contact-type", "security", "--contact", "@-"}, stdin: `{"email":"a@example.com"}`, method: "PATCH", path: "/api/v2/tailnet/-/contacts/security", body: `{"email":"a@example.com"}`},
		{name: "functional options", args: []string{"devices", "list", "--fields", "all", "--filter", "os=linux", "--filter", "os=macos"}, method: "GET", path: "/api/v2/tailnet/-/devices", query: "fields=all&os=linux&os=macos", response: `{"devices":[]}`, output: "[]\n"},
		{name: "optional pointers absent", args: []string{"users", "list"}, method: "GET", path: "/api/v2/tailnet/-/users", response: `{"users":[]}`, output: "[]\n"},
		{name: "optional pointer present", args: []string{"users", "list", "--role", "admin"}, method: "GET", path: "/api/v2/tailnet/-/users", query: "role=admin", response: `{"users":[]}`, output: "[]\n"},
		{name: "optional enum type", args: []string{"users", "list", "--user-type", "shared"}, method: "GET", path: "/api/v2/tailnet/-/users", query: "type=shared", response: `{"users":[]}`, output: "[]\n"},
		{name: "logging enum", args: []string{"logging", "logstream-configuration", "--log-type", "configuration"}, method: "GET", path: "/api/v2/tailnet/-/logging/configuration/stream", response: `{}`, output: "{}\n"},
		{name: "raw HuJSON", args: []string{"policy-file", "set", "--acl", "@-"}, stdin: "{// comment\n}", method: "POST", path: "/api/v2/tailnet/-/acl", body: "{// comment\n}", contentType: "application/hujson"},
		{name: "policy HuJSON with string grant app", args: []string{"policy-file", "get"}, method: "GET", path: "/api/v2/tailnet/-/acl", response: "{// comment\n\"grants\":[{\"src\":[\"*\"],\"dst\":[\"*\"],\"app\":\"example\"},],}", output: "{\"grants\":[{\"src\":[\"*\"],\"dst\":[\"*\"],\"app\":\"example\"}]}\n"},
		{name: "policy HuJSON output", args: []string{"policy-file", "get", "--format", "hujson"}, method: "GET", path: "/api/v2/tailnet/-/acl", response: "{// comment\n\"grants\":[],}\n", output: "{// comment\n\"grants\":[],}\n"},
		{name: "policy explicit JSON output", args: []string{"policy-file", "get", "--format", "json"}, method: "GET", path: "/api/v2/tailnet/-/acl", response: "{// comment\n\"grants\":[],}\n", output: "{\"grants\":[]}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tt.method || r.URL.Path != tt.path || r.URL.RawQuery != tt.query {
					t.Errorf("request: %s %s; want %s %s?%s", r.Method, r.URL, tt.method, tt.path, tt.query)
				}
				body, _ := io.ReadAll(r.Body)
				if tt.contentType != "" {
					if r.Header.Get("Content-Type") != tt.contentType {
						t.Errorf("content type: %s", r.Header.Get("Content-Type"))
					}
					if string(body) != tt.body {
						t.Errorf("body %q", body)
					}
				} else if tt.body != "" {
					var got, want any
					if err := json.Unmarshal(body, &got); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(tt.body), &want); err != nil {
						t.Fatal(err)
					}
					if fmt.Sprint(got) != fmt.Sprint(want) {
						t.Errorf("body %s; want %s", body, tt.body)
					}
				}
				if tt.response != "" {
					fmt.Fprint(w, tt.response)
				}
			})
			base, _ := url.Parse("https://api.example.test")
			cmd := newCommand(&tailscale.Client{BaseURL: base, HTTP: &http.Client{Transport: handlerTransport{handler}}})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetIn(strings.NewReader(tt.stdin))
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Errorf("got %d HTTP calls", calls)
			}
			if out.String() != tt.output {
				t.Errorf("output %q; want %q", out.String(), tt.output)
			}
		})
	}
}

func TestInvalidInputDoesNotCallAPI(t *testing.T) {
	for _, args := range [][]string{
		{"devices", "set-authorized", "--device-id", "d1"},
		{"devices", "list", "--filter", "bad"},
		{"devices", "list", "--fields", "bad"},
		{"keys", "create-auth-key", "--ckr", "{"},
		{"policy-file", "get", "--format", "invalid"},
	} {
		cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("expected error for %v", args)
		}
	}
}

type rejectTransport struct{ t *testing.T }

func (r rejectTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("unexpected HTTP call")
	return nil, fmt.Errorf("unexpected HTTP call")
}

type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, r)
	return recorder.Result(), nil
}

func TestNetworkFlowLogs(t *testing.T) {
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v2/tailnet/-/logging/network" || r.URL.Query().Get("start") != "2026-09-20T00:00:00Z" || r.URL.Query().Get("end") != "2026-09-21T00:00:00Z" {
			t.Errorf("unexpected URL: %s", r.URL)
		}
		fmt.Fprint(w, `{"logs":[{"nodeId":"one"},{"nodeId":"two"}]}`)
	})
	cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: handlerTransport{handler}}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"logging", "get-network-flow-logs", "--params", `{"Start":"2026-09-20T00:00:00Z","End":"2026-09-21T00:00:00Z"}`})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || calls != 1 {
		t.Fatalf("calls=%d output=%s", calls, out.String())
	}
	for i, line := range lines {
		var log tailscale.NetworkFlowLog
		if err := json.Unmarshal([]byte(line), &log); err != nil {
			t.Fatal(err)
		}
		if log.NodeID != []string{"one", "two"}[i] {
			t.Errorf("node ID: %s", log.NodeID)
		}
	}
}

func TestInvalidEnumsDoNotCallAPI(t *testing.T) {
	for _, tt := range []struct {
		args          []string
		flag, allowed string
	}{
		{[]string{"contacts", "update", "--contact-type", "bad", "--contact", "{}"}, "contact-type", "security"},
		{[]string{"logging", "logstream-configuration", "--log-type", "bad"}, "log-type", "configuration"},
		{[]string{"users", "list", "--role", "bad"}, "role", "admin"},
		{[]string{"users", "list", "--user-type", "bad"}, "user-type", "member"},
		{[]string{"users", "list", "--role="}, "role", "admin"},
		{[]string{"devices", "list", "--fields", "bad"}, "fields", "default"},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			cmd := newCommand(&tailscale.Client{HTTP: &http.Client{Transport: rejectTransport{t}}})
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "--"+tt.flag+": must be one of ") || !strings.Contains(err.Error(), tt.allowed) {
				t.Fatalf("expected enum error naming flag and allowed values, got %v", err)
			}
		})
	}
}
