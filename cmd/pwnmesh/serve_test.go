//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestServeTrustedHostFlags(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, args := range [][]string{
		nil,
		{"--host", ""},
		{"--host", "0.0.0.0"},
		{"--host", "::1"},
		{"--host", "pwn.example.com"},
		{"--allow-host", "server", "--allow-host", "pwn.example.com, other.example.com"},
	} {
		if err := serve(ctx, args, io.Discard); !errors.Is(err, context.Canceled) {
			t.Errorf("valid flags %v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"--allow-host", ""},
		{"--allow-host", "server,"},
		{"--allow-host", "server:8000"},
		{"--allow-host", "*.example.com"},
		{"--allow-host", "https://pwn.example.com"},
		{"--host", "localhost:8000"},
	} {
		if err := serve(ctx, args, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid trusted host") {
			t.Errorf("invalid flags %v: %v", args, err)
		}
	}
}
