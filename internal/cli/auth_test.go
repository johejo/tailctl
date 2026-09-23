// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"tailscale.com/client/tailscale/v2"
)

// idToken returns a syntactically valid JWT with an unexpired claim and a fake signature.
func idToken(subject string) string {
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "sub": subject})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + "." + base64.RawURLEncoding.EncodeToString([]byte("signature"))
}

func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

func TestAPIKeyWithoutIDTokenSource(t *testing.T) {
	client := newClient(env(map[string]string{"TAILCTL_API_KEY": "tskey-api", "TAILCTL_TAILNET": "example.com"}), nil)
	if client.Auth != nil {
		t.Errorf("auth: %#v; want nil", client.Auth)
	}
	if client.APIKey != "tskey-api" || client.Tailnet != "example.com" {
		t.Errorf("client: %#v", client)
	}
}

func TestIDTokenSourcePrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(idToken("file")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pairs := map[string]string{
		envClientID:    "client",
		envIDToken:     idToken("env"),
		envIDTokenFile: file,
		envIDTokenURL:  "https://idp.example.test/token",
	}
	for _, want := range []string{"env", "file"} {
		client := newClient(env(pairs), nil)
		federation, ok := client.Auth.(*tailscale.IdentityFederation)
		if !ok {
			t.Fatalf("auth: %#v; want identity federation", client.Auth)
		}
		if client.APIKey != "" {
			t.Errorf("API key: %q; want empty", client.APIKey)
		}
		token, err := federation.IDTokenFunc()
		if err != nil {
			t.Fatal(err)
		}
		if subject(t, token) != want {
			t.Errorf("subject %q; want %q", subject(t, token), want)
		}
		delete(pairs, map[string]string{"env": envIDToken, "file": envIDTokenFile}[want])
	}
}

func TestIDTokenFileIsReadOnEveryExchange(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	source := idTokenSource(env(map[string]string{envClientID: "client", envIDTokenFile: file}), nil)
	for _, want := range []string{"first", "second"} {
		if err := os.WriteFile(file, []byte(idToken(want)), 0o600); err != nil {
			t.Fatal(err)
		}
		token, err := source()
		if err != nil {
			t.Fatal(err)
		}
		if subject(t, token) != want {
			t.Errorf("subject %q; want %q", subject(t, token), want)
		}
	}
}

func TestIDTokenJSONKeys(t *testing.T) {
	bare := idToken("bare")
	jsonBare := fmt.Sprintf(`{"id_token":%q}`, bare)
	firstToken := idToken("first")
	secondToken := idToken("second")
	thirdToken := idToken("third")
	fourthToken := idToken("fourth")
	fifthToken := idToken("fifth")
	defaultToken := idToken("default")
	customToken := idToken("custom")
	const invalidFormat = "expected a JSON object holding an ID token or a compact JWT"
	tests := []struct {
		name, keys, data, want, wantErr string
	}{
		{name: "default priority", data: fmt.Sprintf(`{"id_token":%q,"idToken":%q,"value":%q,"token":%q,"access_token":%q}`, firstToken, secondToken, thirdToken, fourthToken, fifthToken), want: firstToken},
		{name: "default idToken", data: fmt.Sprintf(`{"idToken":%q}`, secondToken), want: secondToken},
		{name: "default value", data: fmt.Sprintf(`{"value":%q}`, thirdToken), want: thirdToken},
		{name: "default token", data: fmt.Sprintf(`{"token":%q}`, fourthToken), want: fourthToken},
		{name: "default access_token", data: fmt.Sprintf(`{"access_token":%q}`, fifthToken), want: fifthToken},
		{name: "custom priority", keys: " jwt, ,id_token, ", data: fmt.Sprintf(`{"id_token":%q,"jwt":%q}`, defaultToken, customToken), want: customToken},
		{name: "skip unusable fields", keys: "missing,empty,null,number,object,jwt", data: fmt.Sprintf(`{"empty":"","null":null,"number":42,"object":{},"jwt":%q}`, customToken), want: customToken},
		{name: "replace defaults", keys: "jwt,identity", data: `{"id_token":"default"}`, wantErr: "no ID token in JSON response; looked for jwt, identity"},
		{name: "empty list", keys: " , , ", data: fmt.Sprintf(`{"id_token":%q}`, defaultToken), want: defaultToken},
		{name: "bare token", keys: "jwt", data: " " + bare + "\n", want: bare},
		{name: "invalid JSON token", data: `{"id_token":"garbage"}`, wantErr: `field "id_token" does not contain a compact JWT`},
		{name: "invalid first token does not fall back", data: fmt.Sprintf(`{"id_token":"garbage","token":%q}`, bare), wantErr: `field "id_token" does not contain a compact JWT`},
		{name: "JSON token whitespace", data: fmt.Sprintf(`{"id_token":%q}`, " "+bare+"\n"), want: bare},
		{name: "blank JSON token", data: `{"id_token":" "}`, wantErr: `field "id_token" does not contain a compact JWT`},
		{name: "many segments", data: strings.Repeat(".", maxIDTokenSize), wantErr: invalidFormat},
		{name: "at size limit", data: bare + strings.Repeat(" ", maxIDTokenSize-len(bare)), want: bare},
		{name: "over size limit", data: bare + strings.Repeat(" ", maxIDTokenSize-len(bare)+1), wantErr: fmt.Sprintf("ID token input exceeds %d bytes", maxIDTokenSize)},
		{name: "JSON at size limit", data: jsonBare + strings.Repeat(" ", maxIDTokenSize-len(jsonBare)), want: bare},
		{name: "JSON over size limit", data: jsonBare + strings.Repeat(" ", maxIDTokenSize-len(jsonBare)+1), wantErr: fmt.Sprintf("ID token input exceeds %d bytes", maxIDTokenSize)},
		{name: "plain text", data: "not a token", wantErr: invalidFormat},
		{name: "HTML", data: "<html>Service unavailable</html>", wantErr: invalidFormat},
		{name: "broken JSON", data: `{"id_token":`, wantErr: invalidFormat},
		{name: "JSON array", data: `[]`, wantErr: invalidFormat},
		{name: "JSON string", data: fmt.Sprintf("%q", bare), wantErr: invalidFormat},
		{name: "JSON number", data: `42`, wantErr: invalidFormat},
		{name: "null", data: `null`, wantErr: "no ID token in JSON response; looked for id_token, idToken, value, token, access_token"},
		{name: "extra JWT segment", data: bare + ".extra", wantErr: invalidFormat},
		{name: "missing signature", data: bare[:strings.LastIndex(bare, ".")+1], wantErr: invalidFormat},
		{name: "invalid base64", data: bare + "!", wantErr: invalidFormat},
		{name: "embedded newline", data: bare[:5] + "\n" + bare[5:], wantErr: invalidFormat},
		{name: "non JSON header", data: "aGVhZGVy.e30.c2ln", wantErr: invalidFormat},
		{name: "non JSON claims", data: "e30.Y2xhaW1z.c2ln", wantErr: invalidFormat},
		{name: "null claims", data: "e30.bnVsbA.c2ln", wantErr: invalidFormat},
		{name: "array claims", data: "e30.W10.c2ln", wantErr: invalidFormat},
	}
	for _, sourceName := range []string{envIDToken, envIDTokenFile, envIDTokenURL} {
		t.Run(sourceName, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					pairs := map[string]string{envClientID: "client", envIDTokenJSONKeys: tt.keys}
					switch sourceName {
					case envIDToken:
						pairs[sourceName] = tt.data
					case envIDTokenFile:
						file := filepath.Join(t.TempDir(), "token")
						if err := os.WriteFile(file, []byte(tt.data), 0o600); err != nil {
							t.Fatal(err)
						}
						pairs[sourceName] = file
					case envIDTokenURL:
						pairs[sourceName] = "https://idp.example.test/token"
					}
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						fmt.Fprint(w, tt.data)
					})
					source := idTokenSource(env(pairs), &http.Client{Transport: handlerTransport{handler}})
					got, err := source()
					if tt.wantErr != "" {
						if err == nil || err.Error() != sourceName+": "+tt.wantErr {
							t.Fatalf("error %v; want %s: %s", err, sourceName, tt.wantErr)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if got != tt.want {
						t.Errorf("token %q; want %q", got, tt.want)
					}
				})
			}
		})
	}
}

func TestReadIDTokenStopsAtLimit(t *testing.T) {
	r := strings.NewReader(strings.Repeat(".", 2*maxIDTokenSize))
	data, err := readIDToken(r)
	if err == nil || data != nil {
		t.Fatalf("oversized input: got %d bytes, error %v", len(data), err)
	}
	if got, want := r.Len(), maxIDTokenSize-1; got != want {
		t.Errorf("unread bytes = %d; want %d", got, want)
	}
}

func TestIDTokenURL(t *testing.T) {
	tests := []struct {
		name, rawURL, headers, body, method, path, query, requestBody, contentType, response string
	}{
		{name: "bare token", rawURL: "https://idp.example.test/identity?audience=api.tailscale.com/client", method: "GET", path: "/identity", query: "audience=api.tailscale.com/client", response: idToken("url")},
		{name: "JSON id_token", rawURL: "https://idp.example.test/identity", headers: "Metadata-Flavor: Google", method: "GET", path: "/identity", response: fmt.Sprintf(`{"id_token":%q}`, idToken("url"))},
		{name: "JSON value", rawURL: "https://idp.example.test/identity", method: "GET", path: "/identity", response: fmt.Sprintf(`{"value":%q}`, idToken("url"))},
		{name: "JSON body", rawURL: "http://_api.internal:4280/v1/tokens/oidc", headers: "Content-Type: application/json", body: `{"aud":"api.tailscale.com/client"}`, method: "POST", path: "/v1/tokens/oidc", requestBody: `{"aud":"api.tailscale.com/client"}`, contentType: "application/json", response: idToken("url")},
		{name: "form body", rawURL: "https://idp.example.test/identity-token", headers: "Tsidp-Identity-Token: true\nAccept: application/json", body: "audience=api.tailscale.com/client", method: "POST", path: "/identity-token", requestBody: "audience=api.tailscale.com/client", response: fmt.Sprintf(`{"id_token":%q}`, idToken("url"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || r.URL.Path != tt.path || r.URL.RawQuery != tt.query {
					t.Errorf("request: %s %s; want %s %s?%s", r.Method, r.URL, tt.method, tt.path, tt.query)
				}
				if tt.requestBody != "" {
					if got := readAll(t, r); got != tt.requestBody {
						t.Errorf("body %q; want %q", got, tt.requestBody)
					}
				}
				if got := r.Header.Get("Content-Type"); got != tt.contentType {
					t.Errorf("content type %q; want %q", got, tt.contentType)
				}
				for header := range strings.SplitSeq(tt.headers, "\n") {
					name, value, _ := strings.Cut(header, ":")
					if name != "" && r.Header.Get(name) != strings.TrimSpace(value) {
						t.Errorf("header %s: %q; want %q", name, r.Header.Get(name), strings.TrimSpace(value))
					}
				}
				fmt.Fprint(w, tt.response)
			})
			pairs := map[string]string{envClientID: "client", envIDTokenURL: tt.rawURL, envIDTokenHead: tt.headers, envIDTokenBody: tt.body}
			source := idTokenSource(env(pairs), &http.Client{Transport: handlerTransport{handler}})
			token, err := source()
			if err != nil {
				t.Fatal(err)
			}
			if subject(t, token) != "url" {
				t.Errorf("subject %q; want url", subject(t, token))
			}
		})
	}
}

func TestIDTokenUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket test")
	}
	// Keep the socket path below macOS's Unix socket path length limit.
	dir, err := os.MkdirTemp("/tmp", "tailctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://other.example/token", http.StatusFound)
			return
		}
		if r.URL.Path == "/error" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		if r.URL.EscapedPath() != "/v1/tokens/oidc%2Ftest" || r.URL.Query().Get("aud") != "a/b" || r.Host != "localhost" {
			t.Errorf("unexpected request: host=%s URL=%s", r.Host, r.URL)
		}
		if r.Header.Get("X-Test") != "socket" {
			t.Error("missing custom header")
		}
		if r.Method == http.MethodPost {
			if got := readAll(t, r); got != `{"aud":"api.tailscale.com/client"}` {
				t.Errorf("unexpected body: %s", got)
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("missing content type")
			}
			fmt.Fprintf(w, `{"jwt":%q}`, idToken("post"))
			return
		}
		fmt.Fprint(w, idToken("get"))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	// Socket requests must bypass HTTP proxies.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	base := "http+unix://" + (&url.URL{Path: socket}).EscapedPath() + ":"
	for _, method := range []string{"get", "post"} {
		t.Run(method, func(t *testing.T) {
			pairs := map[string]string{
				envClientID: "client", envIDTokenURL: base + "/v1/tokens/oidc%2Ftest?aud=a%2Fb",
				envIDTokenHead: "X-Test: socket\nContent-Type: application/json", envIDTokenJSONKeys: "jwt",
			}
			if method == "post" {
				pairs[envIDTokenBody] = `{"aud":"api.tailscale.com/client"}`
			}
			source := idTokenSource(env(pairs), nil)
			for range 2 {
				token, err := source()
				if err != nil {
					t.Fatal(err)
				}
				if got := subject(t, token); got != method {
					t.Errorf("subject %q; want %q", got, method)
				}
			}
		})
	}
	for _, tt := range []struct{ path, want string }{
		{"/redirect", "redirects are not supported"},
		{"/error", "403"},
	} {
		source := idTokenSource(env(map[string]string{envClientID: "client", envIDTokenURL: base + tt.path}), nil)
		if _, err := source(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %v; want %q", tt.path, err, tt.want)
		}
	}
	server.Close()
	source := idTokenSource(env(map[string]string{envClientID: "client", envIDTokenURL: base + "/token"}), nil)
	if _, err := source(); err == nil || !strings.Contains(err.Error(), envIDTokenURL) {
		t.Errorf("missing socket: error %v", err)
	}
}

func TestInvalidIDTokenUnixURL(t *testing.T) {
	for _, rawURL := range []string{
		"http+unix:///.fly/api", "http+unix:///.fly/api:", "http+unix:///:/token",
		"http+unix://host/.fly/api:/token", "http+unix:relative:/token",
		"http+unix:///.fly/api:/token#fragment", "http+unix:///.fly/%zz:/token",
	} {
		t.Run(rawURL, func(t *testing.T) {
			source := idTokenSource(env(map[string]string{envClientID: "client", envIDTokenURL: rawURL}), nil)
			if _, err := source(); err == nil || !strings.Contains(err.Error(), envIDTokenURL) {
				t.Errorf("error %v; want URL configuration error", err)
			}
		})
	}
}

func TestIDTokenErrors(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/error":
			http.Error(w, "no identity for you", http.StatusForbidden)
		case "/empty":
		default:
			fmt.Fprint(w, `{"unexpected":"shape"}`)
		}
	})
	tests := []struct {
		name string
		envs map[string]string
		want string
	}{
		{name: "missing client ID", envs: map[string]string{envIDToken: idToken("env")}, want: envClientID},
		{name: "missing file", envs: map[string]string{envClientID: "client", envIDTokenFile: filepath.Join(t.TempDir(), "absent")}, want: "no such file"},
		{name: "HTTP error", envs: map[string]string{envClientID: "client", envIDTokenURL: "https://idp.example.test/error"}, want: "403"},
		{name: "empty response", envs: map[string]string{envClientID: "client", envIDTokenURL: "https://idp.example.test/empty"}, want: "empty ID token"},
		{name: "unknown JSON", envs: map[string]string{envClientID: "client", envIDTokenURL: "https://idp.example.test/json"}, want: "no ID token in JSON response"},
		{name: "malformed header", envs: map[string]string{envClientID: "client", envIDTokenURL: "https://idp.example.test/json", envIDTokenHead: "Metadata-Flavor"}, want: "expected Name: value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := idTokenSource(env(tt.envs), &http.Client{Transport: handlerTransport{handler}})
			_, err := source()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %v; want one containing %q", err, tt.want)
			}
		})
	}
}

// TestIdentityFederationExchange covers the full path: fetch an ID token, trade it
// for an API access token, then authenticate an API call with that token.
func TestIdentityFederationExchange(t *testing.T) {
	var identityCalls, exchangeCalls, apiCalls int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/identity-token":
			identityCalls++
			if r.Header.Get("Tsidp-Identity-Token") != "true" {
				t.Errorf("missing identity token header")
			}
			if got := r.URL.Query().Get("audience"); got != "api.tailscale.com/client" {
				t.Errorf("audience %q", got)
			}
			fmt.Fprintf(w, `{"id_token":%q}`, idToken("federated"))
		case "/api/v2/oauth/token-exchange":
			exchangeCalls++
			values, err := url.ParseQuery(readAll(t, r))
			if err != nil {
				t.Fatal(err)
			}
			if values.Get("client_id") != "client" || subject(t, values.Get("jwt")) != "federated" {
				t.Errorf("exchange request %v", values)
			}
			fmt.Fprint(w, `{"access_token":"tskey-exchanged","token_type":"Bearer","expires_in":3600}`)
		case "/api/v2/tailnet/-/devices":
			apiCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer tskey-exchanged" {
				t.Errorf("authorization %q", got)
			}
			fmt.Fprint(w, `{"devices":[]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	httpClient := &http.Client{Transport: handlerTransport{handler}}
	client := newClient(env(map[string]string{
		envClientID:    "client",
		envIDTokenURL:  "https://idp.example.test/identity-token?audience=api.tailscale.com/client",
		envIDTokenHead: "Tsidp-Identity-Token: true",
	}), httpClient)
	base, _ := url.Parse("https://api.example.test")
	client.BaseURL, client.HTTP = base, httpClient
	cmd := newCommand(client)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"devices", "list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Errorf("output %q", out.String())
	}
	if identityCalls != 1 || exchangeCalls != 1 || apiCalls != 1 {
		t.Errorf("calls: identity=%d exchange=%d api=%d", identityCalls, exchangeCalls, apiCalls)
	}
}

func readAll(t *testing.T, r *http.Request) string {
	t.Helper()
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(r.Body); err != nil {
		t.Fatal(err)
	}
	return body.String()
}

func subject(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q is not a JWT", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims.Subject
}
