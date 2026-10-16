package docker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path"
	"time"

	"pwnmesh/internal/protocol"
)

func (c *Client) SetGraphHandler(handler func(context.Context, protocol.Job, protocol.GraphRequest) (any, error)) {
	c.graphMu.Lock()
	defer c.graphMu.Unlock()
	c.graphHandler = handler
}

func (c *Client) graphBridge(parent context.Context, name, runDir string, j protocol.Job) func(protocol.GraphRequest) error {
	seen := map[string][32]byte{}
	return func(request protocol.GraphRequest) error {
		if !j.GraphRPC {
			return errors.New("graph bridge is disabled for this job")
		}
		if !protocol.ValidGraphRequestID(request.RequestID) {
			return errors.New("invalid graph request_id")
		}
		ctx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		response := protocol.GraphResponse{RequestID: request.RequestID}
		raw, err := json.Marshal(request)
		digest := sha256.Sum256(raw)
		if err == nil {
			err = protocol.ValidateGraphRequest(j, request)
		}
		if previous, ok := seen[request.RequestID]; ok && previous != digest {
			err = errors.New("graph request_id reused for different input")
		}
		if len(seen) >= 4096 {
			err = errors.New("graph bridge request limit reached")
		}
		if err == nil {
			seen[request.RequestID] = digest
			c.graphMu.RLock()
			handler := c.graphHandler
			c.graphMu.RUnlock()
			if handler == nil {
				err = errors.New("dispatcher has no graph handler")
			} else {
				var result any
				result, err = handler(ctx, j, request)
				if err == nil {
					response.Result, err = json.Marshal(result)
					if err == nil && request.Op == "graph_action" {
						response.Result, err = protocol.CompactGraphActionResult(response.Result)
					}
				}
			}
		}
		if err != nil {
			response.Error = err.Error()
			response.Failure = protocol.GraphFailure(err)
			response.Result = nil
		}
		raw, err = json.Marshal(response)
		if err != nil {
			return err
		}
		if len(raw) > protocol.MaxGraphRPCBytes {
			response.Result = nil
			response.Error = "graph response exceeds 128 KiB; request a smaller page"
			response.Failure = &protocol.RPCError{Code: "response_too_large", Detail: response.Error}
			raw, _ = json.Marshal(response)
		}
		// A complete temporary file is uploaded before publishing the response.
		// Every path component comes from the verified run or a 32-hex request ID.
		target := path.Join(runDir, "graph-response-"+request.RequestID+".json")
		return c.publish(ctx, name, target, raw)
	}
}
