// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package generate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedFileIsCurrent(t *testing.T) {
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", "tailscale.com/client/tailscale/v2")
	dir, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Generate(strings.TrimSpace(string(dir)))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../cli/commands_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("generated commands are stale: run go generate ./...")
	}
}

func TestUnsupportedSignature(t *testing.T) {
	dir := t.TempDir()
	source := `package sdk
 type Client struct{}
 type ThingsResource struct{}
 func(c *Client) Things()*ThingsResource{return nil}
 func(r *ThingsResource) Broken(ctx context.Context, fn func())error{return nil}
 `
	if err := os.WriteFile(filepath.Join(dir, "sdk.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Generate(dir)
	if err == nil || !strings.Contains(err.Error(), "Things.Broken: unsupported input type func()") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestKebab(t *testing.T) {
	for in, want := range map[string]string{"DNS": "dns", "deviceID": "device-id", "DevicePosture": "device-posture", "SetOAuthClient": "set-oauth-client", "SetIPv4Address": "set-ipv4-address"} {
		if got := kebab(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestClosedStringEnums(t *testing.T) {
	dir := t.TempDir()
	source := `package sdk
 type Client struct{}
 type ThingsResource struct{}
 type ContactType string
 const (
   ContactAccount ContactType = "account"
   ContactAlias
   ContactSecurity ContactType = "security"
 )
 type OpenString string
 const OpenKnown OpenString = "known"
 func(c *Client) Things()*ThingsResource{return nil}
 func(r *ThingsResource) Update(ctx context.Context, contact *ContactType, open OpenString)error{return nil}
 `
	path := filepath.Join(dir, "sdk.go")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"case tailscale.ContactType, *tailscale.ContactType:",
		"string(tailscale.ContactAccount), string(tailscale.ContactAlias), string(tailscale.ContactSecurity)",
		`readInput[*tailscale.ContactType](cmd, "contact", "string")`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(string(got), "string(tailscale.OpenKnown)") {
		t.Fatal("open string treated as closed enum")
	}
	// A reviewed enum must not silently lose validation when its constants disappear.
	source = strings.Replace(source, "ContactType =", "string =", -1)
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(dir); err == nil {
		t.Fatal("expected error for enum without typed constants")
	}
}
