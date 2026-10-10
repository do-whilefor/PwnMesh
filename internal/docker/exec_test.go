package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExecPreservesOutputAndErrors(t *testing.T) {
	frame := func(stream byte, text string) string {
		var header [8]byte
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
		return string(header[:]) + text
	}
	var oversized [8]byte
	oversized[0] = 1
	binary.BigEndian.PutUint32(oversized[4:], 32<<20+1)
	for _, tc := range []struct {
		name, stream, exit, wantOutput, wantError string
		missingID, startFailure, cancel           bool
	}{
		{name: "stdout only", stream: frame(1, "first") + frame(2, "diagnostic") + frame(1, "second"), wantOutput: "firstsecond"},
		{name: "process failure", stream: frame(1, "partial"), exit: `{"ExitCode":137}`, wantOutput: "partial", wantError: "worker process exited with code 137"},
		{name: "missing exec ID", missingID: true, wantError: "Docker returned an empty exec ID"},
		{name: "start failure", startFailure: true, wantError: "Docker Engine returned HTTP 500"},
		{name: "truncated header", stream: "short", wantError: "unexpected EOF"},
		{name: "truncated body", stream: frame(1, "abc")[:10], wantOutput: "ab", wantError: "EOF"},
		{name: "oversized frame", stream: string(oversized[:]), wantError: "oversized Docker stream frame"},
		{name: "cancel running exec", cancel: true, wantError: "context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &Client{http: &http.Client{Transport: inputTestTransport(func(r *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, ""
				switch r.Method + " " + r.URL.Path {
				case "POST /containers/worker/exec":
					body = `{"Id":"execution"}`
					if tc.missingID {
						body = `{}`
					}
				case "POST /exec/execution/start":
					body = tc.stream
					if tc.startFailure {
						status = http.StatusInternalServerError
					}
				case "GET /exec/execution/json":
					body = tc.exit
					if body == "" {
						body = `{"ExitCode":0}`
					}
					if tc.cancel {
						body = `{"Running":true}`
						cancel()
					}
				default:
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
					status = http.StatusNotFound
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			var output bytes.Buffer
			err := client.exec(ctx, "worker", []string{"fixture"}, nil, &output)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || err.Error() != tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
			if output.String() != tc.wantOutput {
				t.Fatalf("stdout = %q, want %q", output.String(), tc.wantOutput)
			}
		})
	}
}
