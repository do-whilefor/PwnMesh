package config

import (
	"os"
	"strings"
)

// Getenv prefers PwnMesh settings and accepts their legacy environment names.
// An explicitly empty setting disables the legacy fallback.
func Getenv(key string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	if suffix, ok := strings.CutPrefix(key, "PWNMESH_"); ok {
		return os.Getenv("XLOOM_" + suffix)
	}
	return ""
}
