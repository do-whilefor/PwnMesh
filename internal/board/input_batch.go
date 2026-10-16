package board

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// InputBatchFile describes a private, already-staged source. The caller owns
// its lifetime and must not supply a network reader while holding a transaction.
// Fresh submissions verify the bytes; receipt replay never consumes Data.
type InputBatchFile struct {
	Name   string    `json:"name"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256"`
	Data   io.Reader `json:"-"`
}

// AddInputBatch atomically accepts files and an optional accompanying user hint.
// It owns the semantic receipt identity independently of multipart encoding.
func (t *Tx) AddInputBatch(project string, files []InputBatchFile, content, key string) (out []InputFile, err error) {
	if key != "" && !validInputBatchKey(key) {
		return nil, Err(422, "Idempotency key must contain 1 to 128 ASCII letters, digits, underscores or hyphens")
	}
	if !utf8.ValidString(content) || utf8.RuneCountInString(content) > 32768 {
		return nil, Err(422, "Content must be valid UTF-8 and at most 32768 characters")
	}
	content = strings.TrimSpace(content)
	if len(files) == 0 && content == "" {
		return nil, Err(422, "Expected input files or non-empty content")
	}
	if len(files) > MaxProjectInputs {
		return nil, Err(413, "Project input limit is 32 files and 512 MiB")
	}
	var total int64
	for _, file := range files {
		if !ValidInputName(file.Name) || file.Size < 0 {
			return nil, Err(422, "Invalid input file metadata")
		}
		if file.Size > MaxInputBytes || file.Size > MaxProjectInputBytes-total {
			return nil, Err(413, "Input limit is 256 MiB per file and 512 MiB per project")
		}
		digest, decodeErr := hex.DecodeString(file.SHA256)
		if decodeErr != nil || len(digest) != sha256.Size || file.SHA256 != strings.ToLower(file.SHA256) {
			return nil, Err(422, "Invalid input file digest")
		}
		total += file.Size
	}
	if err = t.RequireCurrentProject(project); err != nil {
		return nil, err
	}
	// Preserve the original receipt encoding, including [] for text-only input.
	if files == nil {
		files = []InputBatchFile{}
	}
	canonical, err := json.Marshal(struct {
		Files   []InputBatchFile `json:"files"`
		Content string           `json:"content"`
	}{files, content})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	requestSHA256 := hex.EncodeToString(digest[:])
	if saved, found, err := t.inputBatchReceipt(project, key, requestSHA256); found || err != nil {
		return saved, err
	}
	if _, err = t.transactionQueries.Exec("SAVEPOINT pwnmesh_input_batch"); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_, rollbackErr := t.transactionQueries.Exec("ROLLBACK TO pwnmesh_input_batch")
			err = errors.Join(err, rollbackErr)
		}
		_, releaseErr := t.transactionQueries.Exec("RELEASE pwnmesh_input_batch")
		err = errors.Join(err, releaseErr)
		if err != nil {
			out = nil
		}
	}()
	inputs := make([]InputFile, 0, len(files))
	for _, file := range files {
		if file.Data == nil {
			return nil, errors.New("missing staged input source")
		}
		// Materialize only one file with an exact allocation, then detect growth.
		data := make([]byte, file.Size)
		if _, err := io.ReadFull(file.Data, data); err != nil {
			return nil, err
		}
		var extra [1]byte
		if _, err := io.ReadFull(file.Data, extra[:]); err != io.EOF {
			if err != nil {
				return nil, err
			}
			return nil, errors.New("staged input size mismatch")
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != file.Size || hex.EncodeToString(sum[:]) != file.SHA256 {
			return nil, errors.New("staged input size or digest mismatch")
		}
		input, err := t.AddInput(project, file.Name, data)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}
	if content != "" {
		if _, err = t.AddProjectHint(project, HintInput{Content: content, Creator: "user"}, ""); err != nil {
			return nil, err
		}
	}
	if err = t.saveInputBatchReceipt(project, key, requestSHA256, inputs); err != nil {
		return nil, err
	}
	return inputs, nil
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
