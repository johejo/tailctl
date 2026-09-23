// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"tailscale.com/client/tailscale/v2"
)

// Environment variables for client configuration. A token source is consulted on
// every exchange, so rotated Kubernetes projected tokens are picked up without a
// restart.
const (
	envAPIKey          = "TAILCTL_API_KEY"
	envTailnet         = "TAILCTL_TAILNET"
	envClientID        = "TAILCTL_OAUTH_CLIENT_ID"
	envIDToken         = "TAILCTL_ID_TOKEN"
	envIDTokenFile     = "TAILCTL_ID_TOKEN_FILE"
	envIDTokenURL      = "TAILCTL_ID_TOKEN_URL"
	envIDTokenHead     = "TAILCTL_ID_TOKEN_HEADER"
	envIDTokenBody     = "TAILCTL_ID_TOKEN_BODY"
	envIDTokenJSONKeys = "TAILCTL_ID_TOKEN_JSON_KEYS"
	idTokenTimeout     = 30 * time.Second
	maxIDTokenSize     = 1 << 20
)

// newClient builds a client from the environment. Identity federation is used
// when any ID token source is set; otherwise authentication falls back to an API
// key. httpClient is used to fetch ID tokens and may be nil.
func newClient(getenv func(string) string, httpClient *http.Client) *tailscale.Client {
	client := &tailscale.Client{Tailnet: getenv(envTailnet)}
	if source := idTokenSource(getenv, httpClient); source != nil {
		client.Auth = &tailscale.IdentityFederation{ClientID: getenv(envClientID), IDTokenFunc: source}
		return client
	}
	client.APIKey = getenv(envAPIKey)
	return client
}

func idTokenSource(getenv func(string) string, httpClient *http.Client) func() (string, error) {
	token, file, rawURL := getenv(envIDToken), getenv(envIDTokenFile), getenv(envIDTokenURL)
	if token == "" && file == "" && rawURL == "" {
		return nil
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: idTokenTimeout}
	}
	var names []string
	for name := range strings.SplitSeq(getenv(envIDTokenJSONKeys), ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return func() (string, error) {
		clientID := getenv(envClientID)
		if clientID == "" {
			return "", fmt.Errorf("%s is required for identity federation", envClientID)
		}
		switch {
		case token != "":
			data, err := readIDToken(strings.NewReader(token))
			if err != nil {
				return "", fmt.Errorf("%s: %w", envIDToken, err)
			}
			return parseIDToken(data, envIDToken, names)
		case file != "":
			f, err := os.Open(file)
			if err != nil {
				return "", fmt.Errorf("%s: %w", envIDTokenFile, err)
			}
			defer f.Close()
			data, err := readIDToken(f)
			if err != nil {
				return "", fmt.Errorf("%s: %w", envIDTokenFile, err)
			}
			return parseIDToken(data, envIDTokenFile, names)
		default:
			return fetchIDToken(httpClient, rawURL, getenv(envIDTokenHead), getenv(envIDTokenBody), names)
		}
	}
}

// fetchIDToken requests an ID token over HTTP. The request is a GET unless a body
// is configured. The audience the Tailscale token exchange expects is part of the
// caller's URL or body, so no substitution happens here.
func fetchIDToken(httpClient *http.Client, rawURL, headers, body string, names []string) (string, error) {
	requestURL := rawURL
	if strings.HasPrefix(rawURL, "http+unix:") {
		socket, endpoint, err := unixIDTokenURL(rawURL)
		if err != nil {
			return "", fmt.Errorf("%s: %w", envIDTokenURL, err)
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		}
		defer transport.CloseIdleConnections()
		client := *httpClient
		client.Transport = transport
		// A redirect must not turn a socket request into a request to another host.
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return fmt.Errorf("redirects are not supported for Unix socket token endpoints")
		}
		httpClient = &client
		requestURL = endpoint
	}
	method := http.MethodGet
	var payload io.Reader
	if body != "" {
		method = http.MethodPost
		payload = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, requestURL, payload)
	if err != nil {
		return "", fmt.Errorf("%s: %w", envIDTokenURL, err)
	}
	for header := range strings.SplitSeq(headers, "\n") {
		name, value, ok := strings.Cut(header, ":")
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		if !ok {
			return "", fmt.Errorf("%s: expected Name: value, got %q", envIDTokenHead, header)
		}
		req.Header.Set(name, strings.TrimSpace(value))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", envIDTokenURL, err)
	}
	defer resp.Body.Close()
	data, err := readIDToken(resp.Body)
	if err != nil {
		return "", fmt.Errorf("%s: %w", envIDTokenURL, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("%s: %s returned %d: %s", envIDTokenURL, rawURL, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return parseIDToken(data, envIDTokenURL, names)
}

// unixIDTokenURL separates http+unix:///absolute/socket:/request/path?query.
func unixIDTokenURL(rawURL string) (socket, endpoint string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	socketPath, requestPath, ok := strings.Cut(u.EscapedPath(), ":")
	if u.Scheme != "http+unix" || u.Host != "" || u.User != nil || u.Fragment != "" ||
		!ok || !strings.HasPrefix(socketPath, "/") || len(socketPath) < 2 || !strings.HasPrefix(requestPath, "/") {
		return "", "", fmt.Errorf("expected http+unix:///absolute/socket:/request/path?query")
	}
	socket, err = url.PathUnescape(socketPath)
	if err != nil {
		return "", "", err
	}
	endpoint = "http://localhost" + requestPath
	if u.ForceQuery || u.RawQuery != "" {
		endpoint += "?" + u.RawQuery
	}
	return socket, endpoint, nil
}

// readIDToken reads one extra byte to detect oversized input without truncation.
func readIDToken(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxIDTokenSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxIDTokenSize {
		return nil, fmt.Errorf("ID token input exceeds %d bytes", maxIDTokenSize)
	}
	return data, nil
}

// parseIDToken accepts a bare JWT or a JSON object holding a token, covering the
// response shapes of the common identity providers when names is empty.
func parseIDToken(data []byte, source string, names []string) (string, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return "", fmt.Errorf("%s: empty ID token", source)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		if value := string(data); looksLikeCompactJWT(value) {
			return value, nil
		}
		return "", fmt.Errorf("%s: expected a JSON object holding an ID token or a compact JWT", source)
	}
	if len(names) == 0 {
		names = []string{"id_token", "idToken", "value", "token", "access_token"}
	}
	for _, name := range names {
		var token string
		if err := json.Unmarshal(fields[name], &token); err == nil && token != "" {
			token = strings.TrimSpace(token)
			if !looksLikeCompactJWT(token) {
				return "", fmt.Errorf("%s: field %q does not contain a compact JWT", source, name)
			}
			return token, nil
		}
	}
	return "", fmt.Errorf("%s: no ID token in JSON response; looked for %s", source, strings.Join(names, ", "))
}

// looksLikeCompactJWT recognizes the shape of a signed compact JWT. It does not
// validate JOSE headers, signatures, or claims; verification belongs to the
// Tailscale token exchange server.
func looksLikeCompactJWT(value string) bool {
	parts := strings.SplitN(value, ".", 4)
	if len(parts) != 3 {
		return false
	}
	for i, part := range parts {
		if part == "" || strings.ContainsAny(part, "\r\n") {
			return false
		}
		decoded, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			return false
		}
		if i < 2 {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(decoded, &object); err != nil || object == nil {
				return false
			}
		}
	}
	return true
}
