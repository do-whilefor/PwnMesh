package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	b "pwnmesh/internal/board"
)

// A supplement becomes visible only after every file and its accompanying
// instruction have arrived. Spool first so slow clients neither hold the store
// transaction nor keep an entire 512 MiB batch in memory.
func (s *Server) uploadInputBatch(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-PwnMesh-Run") != "" {
		writeError(w, r, b.Err(403, "Input upload is a project-management operation"))
		return
	}
	deadline := time.Now().Add(5 * time.Minute)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	r.Body = http.MaxBytesReader(w, r.Body, b.MaxProjectInputBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, r, b.Err(422, "Expected multipart form with input files"))
		return
	}
	files := []b.InputBatchFile{}
	var total int64
	var content, key string
	contentSeen, keySeen := false, false
	for {
		// NextPart decodes quoted-printable data; evidence must retain its bytes.
		part, partErr := mr.NextRawPart()
		if partErr == io.EOF {
			break
		}
		if partErr != nil {
			writeError(w, r, inputBatchReadError(partErr))
			return
		}
		disposition, params, parseErr := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if parseErr != nil || disposition != "form-data" {
			writeError(w, r, b.Err(422, "Expected file, content or idempotency_key form fields"))
			return
		}
		switch params["name"] {
		case "file":
			// Do not use Part.FileName, which silently strips path components.
			name := params["filename"]
			if !b.ValidInputName(name) {
				writeError(w, r, b.Err(422, "Expected an input file with a plain filename"))
				return
			}
			if len(files) >= b.MaxProjectInputs {
				writeError(w, r, b.Err(413, "Project input limit is 32 files and 512 MiB"))
				return
			}
			file, err := os.CreateTemp("", "pwnmesh-input-batch-*")
			if err != nil {
				writeError(w, r, err)
				return
			}
			defer func() {
				file.Close()
				os.Remove(file.Name())
			}()
			// Linux retains unlinked files through their open descriptors. The
			// 32-file limit bounds descriptors; process exit also reclaims bytes.
			if err = os.Remove(file.Name()); err != nil {
				writeError(w, r, err)
				return
			}
			limit := min(int64(b.MaxInputBytes), int64(b.MaxProjectInputBytes)-total)
			hash := sha256.New()
			size, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(part, limit+1))
			if size > limit {
				writeError(w, r, b.Err(413, "Input limit is 256 MiB per file and 512 MiB per project"))
				return
			}
			if copyErr != nil {
				writeError(w, r, inputBatchReadError(copyErr))
				return
			}
			total += size
			if _, err = file.Seek(0, io.SeekStart); err != nil {
				writeError(w, r, err)
				return
			}
			files = append(files, b.InputBatchFile{Name: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), Data: file})
		case "content":
			if _, hasFilename := params["filename"]; contentSeen || hasFilename {
				writeError(w, r, b.Err(422, "Expected at most one text content field"))
				return
			}
			contentSeen = true
			raw, err := io.ReadAll(io.LimitReader(part, 32768*utf8.UTFMax+1))
			if err != nil {
				writeError(w, r, inputBatchReadError(err))
				return
			}
			if !utf8.Valid(raw) || utf8.RuneCount(raw) > 32768 {
				writeError(w, r, b.Err(422, "Content must be valid UTF-8 and at most 32768 characters"))
				return
			}
			content = strings.TrimSpace(string(raw))
		case "idempotency_key":
			if _, hasFilename := params["filename"]; keySeen || hasFilename {
				writeError(w, r, b.Err(422, "Expected at most one text idempotency_key field"))
				return
			}
			keySeen = true
			raw, err := io.ReadAll(io.LimitReader(part, 129))
			if err != nil {
				writeError(w, r, inputBatchReadError(err))
				return
			}
			key = string(raw)
			if key == "" || len(key) > 128 {
				writeError(w, r, b.Err(422, "Idempotency key must contain 1 to 128 ASCII letters, digits, underscores or hyphens"))
				return
			}
		default:
			writeError(w, r, b.Err(422, "Expected file, content or idempotency_key form fields"))
			return
		}
	}
	var inputs []b.InputFile
	err = s.Store.Do(r.Context(), func(t *b.Tx) error {
		var err error
		inputs, err = t.AddInputBatch(r.PathValue("pid"), files, content, key)
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(inputs)
}

func inputBatchReadError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return b.Err(413, "Project input limit is 32 files and 512 MiB")
	}
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return err
	}
	return b.Err(422, "Incomplete input upload")
}
