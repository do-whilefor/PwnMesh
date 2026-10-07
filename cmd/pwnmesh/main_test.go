//go:build linux

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHelpDisplaysBrandAndCompatibleCommand(t *testing.T) {
	for _, alias := range []string{"help", "--help", "-h"} {
		t.Run(alias, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := run(context.Background(), []string{alias}, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); !strings.HasPrefix(got, "PwnMesh\n") || !strings.Contains(got, "Usage: pwnmesh <serve|dispatch|worker> [options]") {
				t.Fatalf("help must identify PwnMesh and its executable: %q", got)
			}
			if errOut.Len() != 0 {
				t.Fatalf("help wrote to stderr: %s", &errOut)
			}
		})
	}
}

func TestMissingCommandDisplaysBrandedUsageOnStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), nil, &out, &errOut); err == nil || err.Error() != "a command is required" {
		t.Fatalf("missing command error = %v", err)
	}
	if out.Len() != 0 || !strings.HasPrefix(errOut.String(), "PwnMesh\n") || !strings.Contains(errOut.String(), "Usage: pwnmesh ") {
		t.Fatalf("unexpected usage streams: stdout=%q stderr=%q", &out, &errOut)
	}
}
