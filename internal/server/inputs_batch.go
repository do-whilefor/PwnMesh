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
	dir, err := os.MkdirTemp("", "pwnmesh-input-batch-*")
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer os.RemoveAll(dir)
	type stagedFile struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		path   string
	}
	files := []stagedFile{}
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
			file, err := os.CreateTemp(dir, "input-*")
			if err != nil {
				writeError(w, r, err)
				return
			}
			limit := min(int64(b.MaxInputBytes), int64(b.MaxProjectInputBytes)-total)
			hash := sha256.New()
			size, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(part, limit+1))
			closeErr := file.Close()
			if size > limit {
				writeError(w, r, b.Err(413, "Input limit is 256 MiB per file and 512 MiB per project"))
				return
			}
			if copyErr != nil {
				writeError(w, r, inputBatchReadError(copyErr))
				return
			}
			if closeErr != nil {
				writeError(w, r, closeErr)
				return
			}
			total += size
			files = append(files, stagedFile{Name: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), path: file.Name()})
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
			if !validInputBatchKey(key) {
				writeError(w, r, b.Err(422, "Idempotency key must contain 1 to 128 ASCII letters, digits, underscores or hyphens"))
				return
			}
		default:
			writeError(w, r, b.Err(422, "Expected file, content or idempotency_key form fields"))
			return
		}
	}
	if len(files) == 0 && content == "" {
		writeError(w, r, b.Err(422, "Expected input files or non-empty content"))
		return
	}
	// Bind semantic input, excluding temporary paths and multipart boundaries.
	// File order is preserved because it also determines the receipt's order.
	canonical, _ := json.Marshal(struct {
		Files   []stagedFile `json:"files"`
		Content string       `json:"content"`
	}{files, content})
	digest := sha256.Sum256(canonical)
	requestSHA256 := hex.EncodeToString(digest[:])
	inputs := make([]b.InputFile, 0, len(files))
	err = s.Store.Do(r.Context(), func(t *b.Tx) error {
		project := r.PathValue("pid")
		graph, err := t.Load(project)
		if err != nil {
			return err
		}
		// Multipart requests do not pass through wrap. Text-only batches also
		// skip AddInput, so enforce the same historical read-only boundary here.
		if graph.Project.OrchestrationVersion != 1 {
			return b.Err(409, "Historical projects are read-only; create a new project to continue")
		}
		if saved, found, err := t.InputBatchReceipt(project, key, requestSHA256); found || err != nil {
			inputs = saved
			return err
		}
		for _, file := range files {
			data, err := os.ReadFile(file.path)
			if err != nil {
				return err
			}
			input, err := t.AddInput(project, file.Name, data)
			if err != nil {
				return err
			}
			inputs = append(inputs, input)
		}
		if content != "" {
			if _, _, err := s.hint(t, &request{fields: map[string]any{"content": content, "creator": "user"}}, r); err != nil {
				return err
			}
		}
		return t.SaveInputBatchReceipt(project, key, requestSHA256, inputs)
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

func validInputBatchKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, ch := range key {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
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
