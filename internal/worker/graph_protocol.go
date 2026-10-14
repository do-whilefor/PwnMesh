package worker

import "pwnmesh/internal/protocol"

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
