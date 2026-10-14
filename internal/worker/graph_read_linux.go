//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
)

// A model read can span several small RPC frames. Keep the complete logical
// result bounded independently of the transport, without truncating records.
const maxModelGraphReadBytes = 1 << 20

type modelGraphRecordMarker struct {
	graphRecordReference
	ID              string `json:"id"`
	EvidenceOmitted bool   `json:"evidence_omitted"`
	SourcesOmitted  bool   `json:"sources_omitted"`
	EvidenceCount   int    `json:"evidence_count"`
	SourcesCount    int    `json:"sources_count"`
}

func graphRecordShape(section string, raw json.RawMessage) bool {
	if section == "sources" {
		var id string
		return json.Unmarshal(raw, &id) == nil && id != ""
	}
	var record map[string]json.RawMessage
	return json.Unmarshal(raw, &record) == nil && len(record) > 0
}

// Preserve full judgment fields and explicit support counts, never a truncated
// record or a transport continuation. The omitted arrays remain losslessly
// available through the existing evidence/sources record pages.
func modelGraphSupportMetadata(section string, item json.RawMessage, marker modelGraphRecordMarker) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if !slices.Contains([]string{"facts", "findings", "candidates"}, section) || marker.ID == "" || marker.RecordOmitted ||
		marker.RecordBytes <= len(item) || !graphDigest(marker.RecordVersion) || json.Unmarshal(item, &fields) != nil ||
		marker.EvidenceOmitted && (marker.EvidenceCount <= 0 || fields["evidence"] != nil) ||
		marker.SourcesOmitted && (marker.SourcesCount <= 0 || fields["sources"] != nil || section == "facts") {
		return nil, errors.New("invalid graph support omission")
	}
	delete(fields, "record_bytes")
	delete(fields, "record_version")
	return json.Marshal(fields)
}

// A metadata-only result must survive compaction, but it is not an observation
// of the omitted support that can authorize rebuilding a stale decision.
func (d *decisionDraft) observeGraphRead(section, raw string) {
	var page struct {
		Items []modelGraphRecordMarker `json:"items"`
	}
	if section != "overview" && section != "" && section != "sources" && json.Unmarshal([]byte(raw), &page) == nil && slices.ContainsFunc(page.Items, func(item modelGraphRecordMarker) bool { return item.EvidenceOmitted || item.SourcesOmitted }) {
		if d.reread && d.overviewRead {
			d.refreshData = append(d.refreshData, "<decision_refresh>\n"+raw+"\n</decision_refresh>")
		}
		return
	}
	d.observeRead(section, raw)
}

func graphReadArguments(raw json.RawMessage, op string) (GraphRequest, error) {
	var input struct {
		Section  string   `json:"section"`
		IDs      []string `json:"ids"`
		AssetIDs []string `json:"asset_ids"`
		Offset   int      `json:"offset"`
		Limit    int      `json:"limit"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return GraphRequest{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return GraphRequest{}, errors.New("graph read requires one JSON object")
	}
	return GraphRequest{Op: op, Section: input.Section, IDs: input.IDs, AssetIDs: input.AssetIDs, Offset: input.Offset, Limit: input.Limit}, nil
}

// Read transport pages under one state fence. The caller records only the
// assembled result as a decision observation; hidden fragments are not evidence
// that the model has reviewed a refreshed overview or its supporting records.
func modelGraphRead(ctx context.Context, r GraphRequest, request func(context.Context, GraphRequest) (string, error)) (string, error) {
	if r.Section == "" {
		r.Section = "overview"
	}
	if r.Limit == 0 {
		r.Limit = 20
	}
	ctx = withInternalGraphRead(ctx)
	raw, err := request(ctx, r)
	if err != nil {
		return "", err
	}
	if r.Section == "overview" {
		if !graphRecordShape(r.Section, json.RawMessage(raw)) {
			return "", errors.New("invalid graph overview")
		}
		var reference graphRecordReference
		if err := json.Unmarshal([]byte(raw), &reference); err != nil {
			return "", err
		}
		if reference.RecordOmitted {
			r.ExpectedVersion = reference.StateVersion
			complete, err := readGraphRecord(ctx, r, reference, maxModelGraphReadBytes, request)
			return string(complete), err
		}
		if len(raw) > maxModelGraphReadBytes {
			return "", errors.New("graph overview exceeds the model read budget")
		}
		return raw, nil
	}
	var result graphPage
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return "", err
	}
	if result.Section != r.Section || result.Total < 0 || result.Offset != min(r.Offset, result.Total) || !graphDigest(result.StateVersion) {
		return "", errors.New("invalid graph read page")
	}
	r.ExpectedVersion = result.StateVersion
	page := result
	result.Items = []json.RawMessage{}
	result.NextOffset = nil
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if len(page.Items) > r.Limit-len(result.Items) || page.Offset+len(page.Items) > result.Total ||
			(page.NextOffset != nil && (*page.NextOffset != page.Offset+len(page.Items) || *page.NextOffset >= result.Total || len(page.Items) == 0)) ||
			(page.NextOffset == nil && page.Offset+len(page.Items) != result.Total) {
			return "", errors.New("invalid graph read continuation offset")
		}
		for index, item := range page.Items {
			if !graphRecordShape(r.Section, item) {
				return "", errors.New("invalid graph record shape")
			}
			var marker modelGraphRecordMarker
			// Support pages may contain source ID strings, not object records.
			if len(item) > 0 && item[0] == '{' {
				if err := json.Unmarshal(item, &marker); err != nil {
					return "", err
				}
			}
			offset := page.Offset + index
			var metadata json.RawMessage
			if marker.EvidenceOmitted || marker.SourcesOmitted {
				metadata, err = modelGraphSupportMetadata(r.Section, item, marker)
				if err != nil {
					return "", err
				}
			}
			if marker.RecordOmitted || marker.EvidenceOmitted || marker.SourcesOmitted {
				read := r
				read.Offset, read.Limit = offset, 1
				if marker.RecordOmitted && (marker.RecordOffset != offset || marker.StateVersion != result.StateVersion) {
					return "", errors.New("graph record continuation changed its bound page")
				}
				if metadata != nil && marker.RecordBytes > maxModelGraphReadBytes {
					item = metadata
				} else {
					item, err = readGraphRecord(ctx, read, marker.graphRecordReference, maxModelGraphReadBytes, request)
					if err != nil {
						return "", err
					}
				}
				if marker.ID != "" {
					var identity struct{ ID string }
					if json.Unmarshal(item, &identity) != nil || identity.ID != marker.ID {
						return "", errors.New("graph record continuation changed its identity")
					}
				}
			}
			result.Items = append(result.Items, item)
			next := offset + 1
			result.NextOffset = nil
			if next < result.Total {
				result.NextOffset = &next
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				return "", err
			}
			if len(encoded) > maxModelGraphReadBytes {
				// A full record can fit by itself but exceed the page budget once
				// its envelope is included. Keep its metadata accessible as well.
				if len(result.Items) == 1 && metadata != nil {
					result.Items[0] = metadata
					encoded, err = json.Marshal(result)
					return string(encoded), err
				}
				result.Items = result.Items[:len(result.Items)-1]
				if len(result.Items) == 0 {
					return "", errors.New("graph record exceeds the model read budget; select its evidence or sources separately")
				}
				result.NextOffset = &offset
				encoded, err = json.Marshal(result)
				return string(encoded), err
			}
		}
		if len(result.Items) == r.Limit || page.NextOffset == nil {
			encoded, err := json.Marshal(result)
			return string(encoded), err
		}
		next := r
		next.Offset = *page.NextOffset
		next.Limit = r.Limit - len(result.Items)
		raw, err = request(ctx, next)
		if err != nil {
			return "", err
		}
		// A fresh page owns fresh RawMessage buffers. Reusing the previous page
		// would overwrite records already retained in the assembled result.
		page = graphPage{}
		if json.Unmarshal([]byte(raw), &page) != nil || page.Section != result.Section || page.Offset != next.Offset || page.StateVersion != result.StateVersion || page.Total != result.Total || page.Revision != result.Revision || page.Generation != result.Generation || !slices.Equal(page.MissingIDs, result.MissingIDs) {
			return "", errors.New("graph read continuation changed its bound page")
		}
	}
}

// Both model reads and authorized evidence lookups use the same fragment
// binding checks, while retaining their independent logical size limits.
func readGraphRecord(ctx context.Context, r GraphRequest, reference graphRecordReference, maxBytes int, request func(context.Context, GraphRequest) (string, error)) (json.RawMessage, error) {
	bound := reference.RecordOmitted || reference.RecordBytes != 0 || reference.RecordVersion != ""
	if bound && (reference.RecordBytes <= 0 || reference.RecordBytes > maxBytes || !graphDigest(reference.RecordVersion)) {
		return nil, errors.New("invalid or oversized graph record; select narrower evidence or sources")
	}
	var result json.RawMessage
	total, version := reference.RecordBytes, reference.RecordVersion
	for offset := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.ByteOffset, r.RecordVersion = &offset, version
		raw, err := request(ctx, r)
		if err != nil {
			return nil, err
		}
		var part graphContentPage
		if json.Unmarshal([]byte(raw), &part) != nil || part.Section != r.Section || part.Offset != r.Offset || part.StateVersion != r.ExpectedVersion || !graphDigest(part.RecordVersion) || part.ByteOffset != offset || part.TotalBytes <= 0 || part.TotalBytes > maxBytes || len(part.Content) == 0 || len(part.Content) > part.TotalBytes-offset {
			return nil, errors.New("invalid graph record continuation")
		}
		if offset == 0 && !bound {
			total, version = part.TotalBytes, part.RecordVersion
		}
		if part.TotalBytes != total || part.RecordVersion != version {
			return nil, errors.New("graph record continuation changed its bound record")
		}
		result = append(result, part.Content...)
		offset += len(part.Content)
		if part.NextByteOffset == nil {
			if offset != total || graphRecordVersion(result) != version || !graphRecordShape(r.Section, result) {
				return nil, errors.New("graph record continuation failed its content check")
			}
			return result, nil
		}
		if *part.NextByteOffset != offset || offset >= total {
			return nil, errors.New("invalid graph record continuation offset")
		}
	}
}
