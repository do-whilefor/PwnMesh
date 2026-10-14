package worker

import (
	"encoding/json"
	"pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

const MaxGraphRPCBytes = protocol.MaxGraphRPCBytes
const maxGraphPageBytes = protocol.MaxGraphPageBytes
const graphRecordReadMore = protocol.GraphRecordReadMore

type GraphRequest = protocol.GraphRequest
type GraphRequestEvent = protocol.GraphRequestEvent
type GraphResponse = protocol.GraphResponse
type graphPage = protocol.GraphItemsPage
type graphRecordReference = protocol.GraphRecordReference
type graphContentPage = protocol.GraphContentPage
type compactFactPublication = protocol.CompactFactPublication

func graphDigest(value string) bool                        { return protocol.ValidGraphDigest(value) }
func ValidGraphRequestID(id string) bool                   { return protocol.ValidGraphRequestID(id) }
func ValidateGraphRequest(j Job, r GraphRequest) error     { return protocol.ValidateGraphRequest(j, r) }
func GraphPage(s board.State, r GraphRequest) (any, error) { return protocol.GraphPage(s, r) }
func graphRecordVersion(raw []byte) string                 { return protocol.GraphRecordVersion(raw) }
func graphRecordContent(r GraphRequest, version string, raw []byte) (graphContentPage, error) {
	return protocol.GraphRecordContent(r, version, raw)
}
func CompactGraphActionResult(raw json.RawMessage) (json.RawMessage, error) {
	return protocol.CompactGraphActionResult(raw)
}
