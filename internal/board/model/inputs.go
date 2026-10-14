package model

import (
	"encoding/hex"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxProjectInputs = 32

const MaxProjectInputBytes = 512 << 20

const MaxInputBytes = 256 << 20

type InputFile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Path      string `json:"path"`
	CreatedAt string `json:"created_at"`
}

func ValidInputName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 200 && utf8.ValidString(name) &&
		!strings.ContainsAny(name, "/\\") && strings.IndexFunc(name, unicode.IsControl) < 0
}

func (f InputFile) Valid() bool {
	id, e1 := hex.DecodeString(f.ID)
	sum, e2 := hex.DecodeString(f.SHA256)
	return e1 == nil && e2 == nil && len(id) == 32 && len(sum) == 32 &&
		f.ID == strings.ToLower(f.ID) && f.SHA256 == strings.ToLower(f.SHA256) &&
		ValidInputName(f.Name) && f.Size >= 0 && f.Size <= MaxInputBytes &&
		f.Path == path.Join("/workspace/.pwnmesh/inputs", f.ID, f.Name)
}
