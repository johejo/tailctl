# tailctl

A command-line client for the Tailscale API.

## Usage

[Build tailctl](#build-and-development), then set `TAILCTL_API_KEY` in the
environment. Optionally set `TAILCTL_TAILNET` or pass `--tailnet` to select a
tailnet. [Identity federation](#identity-federation) is also supported.

```sh
export TAILCTL_API_KEY=YOUR_API_KEY

./tailctl --help
./tailctl devices list --fields all --filter os=linux
./tailctl devices get --device-id DEVICE_ID
./tailctl users list --role admin
./tailctl keys create-auth-key --ckr @key-request.json
./tailctl policy-file validate --acl @policy.hujson
./tailctl logging get-network-flow-logs --params '{"Start":"2026-09-20T00:00:00Z","End":"2026-09-21T00:00:00Z"}'
```

Use `<command> --help` to see available flags, required arguments, and input and
output types. Command and flag names use kebab-case; some argument names follow
the SDK, such as `--ckr`.

- Repeat list flags for multiple values. Filters use `key=value`, for example
  `--filter os=linux --filter os=macos`.
- Boolean flags can be supplied explicitly as `--authorized=false`.
- Results are JSON on stdout. Network flow logs are streamed as JSON Lines.
  Commands with no result are silent on success.
- Errors go to stderr and cause exit status 1.

## JSON input

JSON flags accept inline JSON, `@file`, or `@-` for standard input. Use at most
one stdin input per invocation. Policy-file commands accept raw HuJSON in the
same forms.

Inputs are validated before calling the API:

- Field names are case-sensitive. Unknown fields, duplicate keys, incorrect
  types, and trailing JSON values are rejected, including in nested objects.
- Values must follow the types, nullability, and allowed enum values shown in
  `--help`. Integers must be in range and use integer notation, without a
  decimal point or exponent.
- JSON fields may be omitted; the API checks required fields and other
  API-specific constraints.

Validation errors identify the flag and JSON path:

```text
--request: $.subscriptions[1]: expected string, got number
--request: $.endpointURL: unknown field; did you mean "endpointUrl"?
```

## JSON schemas

Export JSON Schema (Draft 2020-12) for a JSON input flag or a command's output:

```sh
./tailctl keys create-auth-key schema --input ckr
./tailctl devices set-posture-attribute schema --input request > request.schema.json
./tailctl devices get schema --output > device.schema.json
./tailctl logging get-network-flow-logs schema --output
```

Specify exactly one of `--input <flag>` (without the leading `--` in the flag
name) or `--output`. Use `schema --help` to list available inputs. Raw HuJSON
inputs have no input schema, and commands that are silent on success have no
output schema.

Schema export needs no credentials or required command arguments, reads no
input files or stdin, and makes no API calls. For JSON Lines, the output schema
describes one value per line. Output descriptions distinguish nullable fields
from fields that may be omitted.

Schemas describe structural constraints; an external JSON Schema validator may
not enforce every CLI input rule, such as integer notation and custom date or
duration formats.

## Identity federation

Setting an ID token source enables identity federation, which exchanges an OIDC
ID token for a Tailscale API access token. `TAILCTL_OAUTH_CLIENT_ID` is required
in this mode, and `TAILCTL_API_KEY` is ignored.

[Create a federated identity in Tailscale](https://tailscale.com/docs/features/workload-identity-federation#configure-federated-identities-in-the-admin-console)
and set its client ID as `TAILCTL_OAUTH_CLIENT_ID`. The examples below use the
[`devices:core:read`](https://tailscale.com/docs/reference/trust-credentials#scopes) scope.

| Variable | Purpose |
| --- | --- |
| `TAILCTL_ID_TOKEN` | ID token value |
| `TAILCTL_ID_TOKEN_FILE` | File holding the ID token, for Kubernetes, Azure, and AWS |
| `TAILCTL_ID_TOKEN_URL` | HTTP(S) or Unix socket endpoint serving the ID token |
| `TAILCTL_ID_TOKEN_HEADER` | Request headers as `Name: value`, one per line |
| `TAILCTL_ID_TOKEN_BODY` | Request body; sends POST instead of GET |
| `TAILCTL_ID_TOKEN_JSON_KEYS` | Comma-separated JSON keys to search, in priority order |

Token sources take precedence in this order: value, file, URL. The expected
audience is `api.tailscale.com/<client ID>`; configure it in the provider's token
settings, URL, or request body as appropriate. Request bodies are sent unchanged;
set `Content-Type` explicitly with `TAILCTL_ID_TOKEN_HEADER` when needed.

Sources accept a bare JWT or a JSON object containing one under `id_token`,
`idToken`, `value`, `token`, or `access_token`, in that order. Set
`TAILCTL_ID_TOKEN_JSON_KEYS=jwt,id_token` to override these names. The first
non-empty string is used. Token input, including any JSON wrapper, is limited
to 1 MiB.

### Kubernetes

Use a [projected ServiceAccount token](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#serviceaccount-token-volume-projection).
See [Tailscale's cluster preparation requirements](https://tailscale.com/docs/kubernetes-operator/manage-and-configure/workload-identity-federation#prepare-the-cluster)
for setup.

Pod spec excerpt (replace both occurrences of `CLIENT_ID` with your client ID):

```yaml
spec:
  serviceAccountName: tailctl
  ... # Other Pod settings.
  containers:
    - name: tailctl
      ... # Image and command to run tailctl.
      env:
        - name: TAILCTL_OAUTH_CLIENT_ID
          value: CLIENT_ID
        - name: TAILCTL_ID_TOKEN_FILE
          value: /var/run/secrets/tokens/tailscale
      volumeMounts:
        - name: tailscale-token
          mountPath: /var/run/secrets/tokens
          readOnly: true
  volumes:
    - name: tailscale-token
      projected:
        sources:
          - serviceAccountToken:
              audience: api.tailscale.com/CLIENT_ID
              path: tailscale
```

### Google Cloud metadata server

On a Compute Engine VM, use the
[metadata identity endpoint](https://docs.cloud.google.com/docs/authentication/get-id-token#metadata-server).

```sh
export TAILCTL_OAUTH_CLIENT_ID=CLIENT_ID
audience="api.tailscale.com/${TAILCTL_OAUTH_CLIENT_ID}"
export TAILCTL_ID_TOKEN_URL="http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=${audience}"
export TAILCTL_ID_TOKEN_HEADER='Metadata-Flavor: Google'
./tailctl devices list
```

### GitHub Actions

See GitHub's [OIDC documentation](https://docs.github.com/en/actions/reference/security/oidc)
for provider setup and token requests. Set the repository variable
`TAILCTL_OAUTH_CLIENT_ID` to your federated identity's client ID.

```yaml
name: List Tailscale devices
on:
  workflow_dispatch:
jobs:
  devices:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      id-token: write
    steps:
      ... # Check out the repository, set up Go, and build tailctl.
      - name: List devices
        shell: bash
        env:
          TAILCTL_OAUTH_CLIENT_ID: ${{ vars.TAILCTL_OAUTH_CLIENT_ID }}
        run: |
          audience="api.tailscale.com/${TAILCTL_OAUTH_CLIENT_ID}"
          export TAILCTL_ID_TOKEN_URL="${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=${audience}"
          export TAILCTL_ID_TOKEN_HEADER="Authorization: Bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}"
          ./tailctl devices list
```

### Unix sockets and Fly.io

For HTTP over a Unix socket, use
`http+unix:///absolute/socket:/request/path?query` in `TAILCTL_ID_TOKEN_URL`.
Headers and request bodies work the same way as HTTP(S). Unix socket requests
bypass HTTP proxies and do not follow redirects.

On a Fly Machine, [Fly.io's OIDC endpoint](https://fly.io/docs/security/openid-connect/)
is available through `/.fly/api`:

```sh
export TAILCTL_OAUTH_CLIENT_ID=CLIENT_ID
audience="api.tailscale.com/${TAILCTL_OAUTH_CLIENT_ID}"
export TAILCTL_ID_TOKEN_URL='http+unix:///.fly/api:/v1/tokens/oidc'
export TAILCTL_ID_TOKEN_HEADER='Content-Type: application/json'
export TAILCTL_ID_TOKEN_BODY="{\"aud\":\"${audience}\"}"
./tailctl devices list
```

The SDK caches API access tokens and reuses an unexpired ID token when exchanging
again. When it needs a new ID token, tailctl rereads the configured file or calls
the configured token endpoint. A literal `TAILCTL_ID_TOKEN` cannot refresh itself.

## Build and development

Requires the Go version specified in `go.mod`.

```sh
go build -o tailctl ./cmd/tailctl
```

Commands are generated from the Tailscale Go SDK. After updating the SDK,
regenerate the commands and run the tests:

```sh
go generate ./...
go test ./...
```

Do not edit `internal/cli/commands_gen.go` by hand.

## License

MIT License. See [LICENSE](LICENSE).
