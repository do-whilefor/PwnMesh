package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	b "pwnmesh/internal/board"
)

func (s *Server) registerInputRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /projects/{pid}/inputs", s.wrap(func(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
		files, err := t.InputFiles(r.PathValue("pid"))
		return 200, files, err
	}))
	m.HandleFunc("POST /projects/{pid}/inputs", s.uploadInput)
	m.HandleFunc("GET /projects/{pid}/inputs/{fid}", s.downloadInput)
}

func (s *Server) uploadInput(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-PwnMesh-Run") != "" {
		writeError(w, r, b.Err(403, "Input upload is a project-management operation"))
		return
	}
	// Large APKs share the browser's five-minute upload budget. Keep ordinary
	// API deadlines short, but allow both receiving and acknowledging this body.
	deadline := time.Now().Add(5 * time.Minute)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	r.Body = http.MaxBytesReader(w, r.Body, b.MaxInputBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, r, b.Err(422, "Expected multipart form with one file"))
		return
	}
	// Uploaded evidence must retain its exact bytes; NextPart silently decodes
	// quoted-printable Content-Transfer-Encoding.
	part, err := mr.NextRawPart()
	if err != nil {
		writeError(w, r, b.Err(422, "Expected one file"))
		return
	}
	// Read the original filename, not Part.FileName's silently stripped path.
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	name := params["filename"]
	if err != nil || part.FormName() != "file" || !b.ValidInputName(name) {
		writeError(w, r, b.Err(422, "Expected one file with a plain filename"))
		return
	}
	data, err := io.ReadAll(io.LimitReader(part, b.MaxInputBytes+1))
	var tooLarge *http.MaxBytesError
	if len(data) > b.MaxInputBytes || errors.As(err, &tooLarge) {
		writeError(w, r, b.Err(413, "Input file exceeds 256 MiB"))
		return
	}
	if err != nil {
		writeError(w, r, b.Err(422, "Incomplete input upload"))
		return
	}
	if _, err = mr.NextRawPart(); err != io.EOF {
		writeError(w, r, b.Err(422, "Upload exactly one file per request"))
		return
	}
	var file b.InputFile
	err = s.Store.Do(r.Context(), func(t *b.Tx) error { var e error; file, e = t.AddInput(r.PathValue("pid"), name, data); return e })
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(file)
}

func (s *Server) downloadInput(w http.ResponseWriter, r *http.Request) {
	var file b.InputFile
	var data []byte
	err := s.Store.Do(r.Context(), func(t *b.Tx) error {
		var e error
		file, data, e = t.InputData(r.PathValue("pid"), r.PathValue("fid"))
		return e
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-SHA256", file.SHA256)
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	http.ServeContent(w, r, file.Name, time.Time{}, bytes.NewReader(data))
}
