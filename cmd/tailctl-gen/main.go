// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

// tailctl-gen generates commands from the pinned Tailscale SDK.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/johejo/tailctl/internal/generate"
)

func main() {
	dir := flag.String("dir", "", "SDK source directory (default: go list)")
	output := flag.String("out", "internal/cli/commands_gen.go", "output Go file")
	flag.Parse()
	if err := run(*dir, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(dir, out string) error {
	if dir == "" {
		cmd := exec.Command("go", "list", "-f", "{{.Dir}}", "tailscale.com/client/tailscale/v2")
		cmd.Stderr = os.Stderr
		b, err := cmd.Output()
		if err != nil {
			return err
		}
		dir = strings.TrimSpace(string(b))
	}
	b, err := generate.Generate(dir)
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0644)
}
