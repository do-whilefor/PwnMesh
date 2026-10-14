package model

import "encoding/json"

type StateChange struct {
	Revision int64  `json:"revision"`
	Op       string `json:"op"`
	ID       string `json:"id"`
}

type StateEvent struct {
	Revision  int64           `json:"revision"`
	Op        string          `json:"op"`
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	CreatedAt string          `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
	Result    json.RawMessage `json:"result"`
}
