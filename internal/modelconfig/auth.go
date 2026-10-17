package modelconfig

import (
	"encoding/hex"
	"os"
	"path/filepath"
)

// OAuth rotation has a separate writable volume; dispatcher model settings stay read-only.
func AuthDir(settingsPath string) string {
	if dir := os.Getenv("PWNMESH_AUTH_DIR"); dir != "" {
		return dir
	}
	if settingsPath == "" {
		settingsPath, _ = ResolvePath()
	}
	return filepath.Join(filepath.Dir(settingsPath), "model-auth")
}

func ValidCredentialID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
