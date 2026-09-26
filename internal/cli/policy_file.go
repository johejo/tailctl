// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/tailscale/hujson"
	"tailscale.com/client/tailscale/v2"
)

// The SDK's ACL type cannot decode every policy accepted by the API (for
// example, a grant's app field can be a string). Preserve the policy's JSON
// structure instead of decoding it into that type.
func writePolicyFile(cmd *cobra.Command, client *tailscale.Client) error {
	format, err := cmd.Flags().GetString("format")
	if err != nil {
		return err
	}
	if format != "json" && format != "hujson" {
		return fmt.Errorf("--format: expected json or hujson, got %q", format)
	}
	raw, err := client.PolicyFile().Raw(cmd.Context())
	if err != nil {
		return err
	}
	if format == "hujson" {
		_, err = io.WriteString(cmd.OutOrStdout(), raw.HuJSON)
		return err
	}
	standard, err := hujson.Standardize([]byte(raw.HuJSON))
	if err != nil {
		return fmt.Errorf("decode policy file HuJSON: %w", err)
	}
	return writeJSON(cmd, json.RawMessage(standard))
}
