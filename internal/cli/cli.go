// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"tailscale.com/client/tailscale/v2"
)

// NewCommand returns an independent command tree. Credentials are read from the
// environment so tokens need not appear in shell history or process arguments.
func NewCommand() *cobra.Command {
	return newCommand(newClient(os.Getenv, nil))
}

func newCommand(client *tailscale.Client) *cobra.Command {
	root := &cobra.Command{Use: "tailctl", Short: "Tailscale API CLI", SilenceUsage: true, SilenceErrors: true}
	root.SetHelpTemplate(root.HelpTemplate() + `{{with index .Annotations "json-inputs"}}{{.}}{{end}}{{with index .Annotations "json-output"}}{{.}}{{end}}`)
	root.PersistentFlags().StringVar(&client.Tailnet, "tailnet", client.Tailnet, "Tailnet (default: credential's tailnet; env "+envTailnet+")")
	addGeneratedCommands(root, client)
	return root
}

func addInputFlag(cmd *cobra.Command, name, kind, typ string, required bool) {
	help := typ
	switch kind {
	case "bool":
		cmd.Flags().Bool(name, false, help)
	case "strings":
		cmd.Flags().StringArray(name, nil, help+" (repeat flag for each value)")
	case "json":
		cmd.Flags().String(name, "", help+" as JSON, @file, or @- for stdin")
	case "raw":
		cmd.Flags().String(name, "", "HuJSON text, @file, or @- for stdin")
	default:
		cmd.Flags().String(name, "", help)
	}
	if required {
		cmd.Flags().Lookup(name).Usage += " (required)"
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(err)
		}
	}
}

func readInput[T any](cmd *cobra.Command, name, kind string) (T, error) {
	var result T
	if !cmd.Flags().Changed(name) {
		return result, nil
	}
	var value any
	var err error
	switch kind {
	case "string":
		var v string
		if v, err = cmd.Flags().GetString(name); err == nil {
			err = validateEnum(v, enumValues(result))
		}
		value = v
	case "bool":
		value, err = cmd.Flags().GetBool(name)
	case "strings":
		value, err = cmd.Flags().GetStringArray(name)
	default:
		err = fmt.Errorf("unknown input kind %q", kind)
	}
	// Marshalling round-trips the flag value into named and pointer SDK types.
	if err == nil {
		var data []byte
		if data, err = json.Marshal(value); err == nil {
			err = json.Unmarshal(data, &result)
		}
	}
	if err != nil {
		return result, fmt.Errorf("--%s: %w", name, err)
	}
	return result, nil
}

func readContent(cmd *cobra.Command, name string) ([]byte, error) {
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		return nil, err
	}
	if value == "@-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	if after, ok := strings.CutPrefix(value, "@"); ok {
		return os.ReadFile(after)
	}
	return []byte(value), nil
}

func readRaw(cmd *cobra.Command, name string) (string, error) {
	b, err := readContent(cmd, name)
	if err != nil {
		return "", fmt.Errorf("--%s: %w", name, err)
	}
	return string(b), nil
}

func writeJSON(cmd *cobra.Command, v any) error { return json.NewEncoder(cmd.OutOrStdout()).Encode(v) }

// keyValues preserves first-seen key order and repeated values.
type keyValues struct {
	key    string
	values []string
}

func readKeyValues(cmd *cobra.Command, name string) ([]keyValues, error) {
	entries, err := cmd.Flags().GetStringArray(name)
	if err != nil {
		return nil, err
	}
	var result []keyValues
	positions := map[string]int{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--%s: expected key=value, got %q", name, entry)
		}
		pos, exists := positions[key]
		if !exists {
			pos = len(result)
			positions[key] = pos
			result = append(result, keyValues{key: key})
		}
		result[pos].values = append(result[pos].values, value)
	}
	return result, nil
}

func validateEnum(value string, allowed []string) error {
	if len(allowed) > 0 && !slices.Contains(allowed, value) {
		return fmt.Errorf("must be one of %s, got %q", strings.Join(allowed, ", "), value)
	}
	return nil
}
