// Package web embeds the PwnMesh workspace.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var assets embed.FS

// Files contains the workspace's locally bundled static assets.
var Files, _ = fs.Sub(assets, "dist")
