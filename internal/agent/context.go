package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const ContextCheckpointVersion = 1

// Reasoning and visible summary text share the provider's output allowance.
// Keep room for maximum-effort reasoning; SummaryBytes separately bounds the
// text admitted to the request view.
const DefaultSummaryMaxTokens = 16384

// ContextCheckpoint is saved atomically with History. Detailed earlier
// checkpoints remain in the append-only event log, not in the model context.
type ContextCheckpoint struct {
	Version         int    `json:"version"`
	LastSequence    uint64 `json:"last_sequence"`
	CompactionCount uint64 `json:"compaction_count"`
	// OverflowRetries counts recovery attempts since the last saved complete response.
	OverflowRetries int `json:"overflow_retries"`
	// ToolArgumentRetries counts corrections since the last successful tool receipt.
	ToolArgumentRetries int `json:"tool_argument_retries,omitempty"`
	// SummaryRetries is consumed before recovery and reset only on compaction commit.
	SummaryRetries int                   `json:"summary_retries,omitempty"`
	LastCompaction *CompactionCheckpoint `json:"last_compaction,omitempty"`
}

// CompactionCheckpoint retains the metadata needed to continue compaction.
// History already stores the current request view; older views remain in events.
// Legacy checkpoint JSON may contain a view, which decoding safely ignores.
type CompactionCheckpoint struct {
	Version           int    `json:"version"`
	ID                uint64 `json:"id"`
	PreviousID        uint64 `json:"previous_id,omitempty"`
	SourceStart       uint64 `json:"source_start"`
	SourceEnd         uint64 `json:"source_end"`
	AtSequence        uint64 `json:"at_sequence"`
	FirstKeptSequence uint64 `json:"first_kept_sequence,omitempty"`
	Summary           string `json:"summary"`
	// Quotes are copied by Go from tool results, never model text.
	Quotes               []SummaryQuote `json:"quotes,omitempty"`
	BeforeBytes          int            `json:"before_bytes"`
	AfterBytes           int            `json:"after_bytes"`
	BeforeTokensEstimate int            `json:"before_tokens_estimate"`
	AfterTokensEstimate  int            `json:"after_tokens_estimate"`
	TokenBasis           string         `json:"token_basis"`
	Usage                *Usage         `json:"usage,omitempty"`
	Reason               string         `json:"reason"`
	Status               string         `json:"status"`
}

type CompactionRecord struct {
	CompactionCheckpoint
	// View makes this event self-contained for request-view replay even after
	// later compactions. Sequence 0 denotes synthetic pinned/summary messages.
	View []Message `json:"view"`
}

func (l *Loop) initCheckpoint() error {
	if l.Checkpoint != nil {
		if l.Checkpoint.Version != ContextCheckpointVersion {
			return errors.New("unsupported context checkpoint version")
		}
		if l.Checkpoint.OverflowRetries < 0 || l.Checkpoint.OverflowRetries > 1 {
			return errors.New("invalid context overflow recovery allowance")
		}
		if l.Checkpoint.ToolArgumentRetries < 0 || l.Checkpoint.ToolArgumentRetries > 1 {
			return errors.New("invalid tool argument recovery allowance")
		}
		if l.Checkpoint.SummaryRetries < 0 || l.Checkpoint.SummaryRetries > 1 {
			return errors.New("invalid summary recovery allowance")
		}
		for _, m := range l.History {
			if m.Sequence > l.Checkpoint.LastSequence {
				return errors.New("context sequence exceeds checkpoint")
			}
		}
		return nil
	}
	l.Checkpoint = &ContextCheckpoint{Version: ContextCheckpointVersion}
	for i := range l.History {
		if l.History[i].Sequence == 0 {
			l.History[i].Sequence = l.Checkpoint.LastSequence + 1
		}
		if l.History[i].Sequence <= l.Checkpoint.LastSequence {
			return errors.New("invalid transcript sequence")
		}
		l.Checkpoint.LastSequence = l.History[i].Sequence
	}
	return nil
}

func (l *Loop) definitions() []Definition {
	var defs []Definition
	if l.ToolsDisabledReason == "" {
		for _, t := range l.Tools {
			defs = append(defs, t.Definition)
		}
	}
	return defs
}

// WireHistory deliberately drops local usage, sequence and stop metadata.
func WireHistory(messages []Message) []Message {
	result := make([]Message, len(messages))
	for i, m := range messages {
		result[i] = Message{Role: m.Role, Content: m.Content}
		copied := false
		for n, b := range m.Content {
			if b.Type != "tool_use" || objectArguments(b.Input) {
				continue
			}
			// Retained evidence is immutable. Normalize only the request view,
			// retaining tool IDs/results and never replaying historical effects.
			if !copied {
				result[i].Content = append([]Block(nil), m.Content...)
				copied = true
			}
			result[i].Content[n].Input = json.RawMessage(`{}`)
			result[i].Content = append(result[i].Content, Block{Type: "text", Text: fmt.Sprintf("Historical tool arguments for %q were invalid; {} is a protocol placeholder. Use the original transcript for execution evidence.", b.ID)})
		}
	}
	return result
}

func (l *Loop) inputBytes(messages []Message, defs []Definition) (int, error) {
	if sizer, ok := l.Provider.(RequestSizer); ok {
		return sizer.InputBytes(cloneMessages(messages), cloneDefinitions(defs))
	}
	raw, err := json.Marshal(struct {
		Messages []Message    `json:"messages"`
		Tools    []Definition `json:"tools,omitempty"`
	}{WireHistory(messages), defs})
	return len(raw), err
}

// Usage is valid only for the request generation that produced it. Kept
// pre-compaction responses cannot describe the newly shortened context.
func (l *Loop) tokenEstimate(messages []Message, size int, cutoff uint64) (int, string) {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Sequence <= cutoff || m.Usage == nil || m.Usage.Input() <= 0 {
			continue
		}
		trailing, _ := json.Marshal(WireHistory(messages[i+1:]))
		return m.Usage.Input() + m.Usage.OutputTokens + (len(trailing)+2)/3, "provider_usage_plus_estimated_tail"
	}
	return (size + 2) / 3, "request_bytes_div_3_estimate"
}

func budgetError(message string) error {
	return &ModelError{Kind: ErrorBudget, Err: errors.New(message)}
}

// The size belongs only to this prepared request. Reuse it for observation in
// the same turn, avoiding a second full serialization without caching history.
func (l *Loop) compactRequest(ctx context.Context, force bool) (size int, resultErr error) {
	if !force && l.ContextBytes <= 0 && l.ContextTokens <= 0 {
		return -1, nil
	}
	if err := l.initCheckpoint(); err != nil {
		return -1, err
	}
	defs := l.definitions()
	before, err := l.inputBytes(l.History, defs)
	if err != nil {
		return -1, err
	}
	var cutoff uint64
	if l.Checkpoint.LastCompaction != nil {
		cutoff = l.Checkpoint.LastCompaction.AtSequence
	}
	beforeTokens, basis := l.tokenEstimate(l.History, before, cutoff)
	if !force && (l.ContextBytes <= 0 || before <= l.ContextBytes) && (l.ContextTokens <= 0 || beforeTokens <= l.ContextTokens) {
		return before, nil
	}
	if l.TaskPrompt == "" && len(l.History) > 0 {
		l.TaskPrompt = l.History[0].Text()
	}
	inputLimit := l.ContextBytes
	if inputLimit <= 0 {
		inputLimit = before * 3 / 4
	}
	if force && inputLimit >= before {
		inputLimit = before * 3 / 4
	}
	if l.ContextTokens > 0 && inputLimit > l.ContextTokens*3 {
		inputLimit = l.ContextTokens * 3
	}
	// The summary still reads under the original input allowance. A smaller
	// retained-context target must not also discard its source evidence.
	target := inputLimit
	targetTokens := l.ContextTokens
	if l.ContextTargetTokens > 0 {
		if targetTokens <= 0 || l.ContextTargetTokens < targetTokens {
			targetTokens = l.ContextTargetTokens
		}
		if target > targetTokens*3 {
			target = targetTokens * 3
		}
	}
	if target <= 0 {
		return -1, budgetError("input budget is too small for pinned instructions")
	}
	// Runtime task data remains verbatim too. Exclude exact copies and obsolete
	// runtime snapshots from the summary/tail, charging current data to the budget.
	instructions := append([]string(nil), l.PinnedInstructions...)
	pinned := map[string]bool{l.TaskPrompt: true}
	for _, instruction := range instructions {
		pinned[instruction] = true
	}
	var contextData []string
	for _, data := range l.ContextData {
		if data != "" && !pinned[data] {
			contextData = append(contextData, data)
			pinned[data] = true
		}
	}
	body := make([]Message, 0, len(l.History))
	for _, m := range l.History {
		if m.Role == "user" && len(m.Content) == 1 && m.Content[0].Type == "text" {
			text := m.Content[0].Text
			omit := pinned[text]
			for _, prefix := range l.ContextDataPrefixes {
				omit = omit || prefix != "" && strings.HasPrefix(text, prefix)
			}
			if omit {
				continue
			}
		}
		body = append(body, m)
	}
	view := func(summary string, tail []Message) []Message {
		out := []Message{}
		if l.TaskPrompt != "" {
			out = append(out, Text("user", l.TaskPrompt))
		}
		out = append(out, Text("user", summaryViewPrefix+summary))
		out = append(out, tail...)
		for _, data := range contextData {
			out = append(out, Text("user", data))
		}
		for _, instruction := range instructions {
			if instruction != "" {
				out = append(out, Text("user", instruction))
			}
		}
		return out
	}
	fixed, err := l.inputBytes(view("", nil), defs)
	if err != nil {
		return -1, err
	}
	available := target - fixed
	if available < 64 {
		return -1, budgetError("pinned task, runtime data, phase instructions and tool definitions exceed the input budget")
	}
	summaryBudget := l.SummaryBytes
	if summaryBudget <= 0 {
		summaryBudget = available / 3
		if summaryBudget > 12000 {
			summaryBudget = 12000
		}
	}
	if summaryBudget > available {
		return -1, budgetError("summary allowance exceeds remaining input budget")
	}
	// The memory is already JSON when resolveSummary returns it, then is
	// escaped once more inside a message text string. Quotes and backslashes
	// can therefore double its wire size. Reserve this before selecting the
	// tail, so displaced evidence is summarized instead of discarded later.
	summaryBudget = min(summaryBudget, available/2)
	summaryWireBudget := summaryBudget * 2
	recentBudget := l.RecentBytes
	if recentBudget <= 0 {
		recentBudget = available / 2
		if l.ContextTargetTokens > 0 {
			// Fill the target with complete recent groups instead of halving
			// it again, reserving the summary's escaped wire representation.
			recentBudget = available - summaryWireBudget
		}
	}
	if recentBudget > available-summaryWireBudget {
		recentBudget = available - summaryWireBudget
	}
	// Choose a suffix of whole messages/tool groups by bytes. A single huge
	// tool group may be summarized in full instead of defeating compaction.
	cut, err := recentHistoryStart(body, recentBudget)
	if err != nil {
		return -1, err
	}
	if cut == 0 && len(body) > 0 {
		cut = 1
		if cut < len(body) && hasResults(body[cut]) {
			cut++
		}
	}
	if cut == 0 {
		return -1, budgetError("no history can be compacted without changing pinned instructions")
	}
	sourceStart, sourceEnd := uint64(0), uint64(0)
	previousID := uint64(0)
	if old := l.Checkpoint.LastCompaction; old != nil {
		sourceStart = old.SourceStart
		sourceEnd = old.SourceEnd
		previousID = old.ID
	}
	for _, m := range body[:cut] {
		if m.Sequence > 0 {
			if sourceStart == 0 || m.Sequence < sourceStart {
				sourceStart = m.Sequence
			}
			if m.Sequence > sourceEnd {
				sourceEnd = m.Sequence
			}
		}
	}
	attemptRecord := &CompactionRecord{CompactionCheckpoint: CompactionCheckpoint{
		Version: ContextCheckpointVersion, ID: l.Checkpoint.CompactionCount + 1,
		PreviousID: previousID, SourceStart: sourceStart, SourceEnd: sourceEnd,
		AtSequence: l.Checkpoint.LastSequence, BeforeBytes: before,
		BeforeTokensEstimate: beforeTokens, TokenBasis: basis,
		Reason: "threshold", Status: "failed",
	}}
	if force {
		attemptRecord.Reason = "overflow"
	}
	defer func() {
		if resultErr != nil {
			l.emit(Event{Type: "context_compaction_failed", Error: resultErr.Error(), Compaction: attemptRecord})
		}
	}()
	summaryInput, sources, err := l.summaryRequest(body[:cut], inputLimit, summaryBudget)
	if err != nil {
		return -1, err
	}
	maxTokens := l.SummaryMaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultSummaryMaxTokens
	}
	if l.Checkpoint.SummaryRetries > 0 {
		return -1, budgetError("context summary recovery allowance exhausted")
	}
	summary, err := l.generate(ctx, summaryInput, nil, maxTokens)
	attemptRecord.Usage = cloneUsage(summary.Usage)
	if err != nil {
		return -1, fmt.Errorf("context summary: %w", err)
	}
	if summary.StopReason == "max_tokens" || summary.StopReason == "length" {
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		// Persist before sending the retry. A crash or failed retry cannot
		// grant another attempt, extend the deadline, or replace original data.
		l.Checkpoint.SummaryRetries++
		if err := l.saveState(); err != nil {
			l.Checkpoint.SummaryRetries--
			return -1, err
		}
		const guidance = "The previous summary hit its output limit. Rewrite it more briefly with only essential notes and quotes, using the same JSON contract.\n"
		encoded, _ := json.Marshal(guidance)
		summaryInput, sources, err = l.summaryRequest(body[:cut], inputLimit-(len(encoded)-2), summaryBudget)
		if err != nil {
			return -1, err
		}
		summaryInput[0].Content[0].Text = guidance + summaryInput[0].Text()
		if size, err := l.inputBytes(summaryInput, nil); err != nil {
			return -1, err
		} else if size > inputLimit {
			return -1, budgetError("summary retry input exceeds its allowance")
		}
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		summary, err = l.generate(ctx, summaryInput, nil, maxTokens)
		if summary.Usage != nil {
			if attemptRecord.Usage == nil {
				attemptRecord.Usage = &Usage{}
			}
			attemptRecord.Usage.InputTokens += summary.Usage.InputTokens
			attemptRecord.Usage.OutputTokens += summary.Usage.OutputTokens
			attemptRecord.Usage.CacheReadTokens += summary.Usage.CacheReadTokens
			attemptRecord.Usage.CacheWriteTokens += summary.Usage.CacheWriteTokens
		}
		if err != nil {
			return -1, fmt.Errorf("context summary retry: %w", err)
		}
	}
	if summary.Role != "assistant" || summary.StopReason == "max_tokens" || summary.StopReason == "length" {
		return -1, budgetError("context summary was invalid or truncated")
	}
	for _, b := range summary.Content {
		if b.Type == "tool_use" {
			return -1, budgetError("context summary attempted to call a tool")
		}
	}
	text, quotes, err := resolveSummary(summary.Text(), sources, summaryBudget)
	if err != nil {
		return -1, budgetError("context summary: " + err.Error())
	}
	candidate := view(text, body[cut:])
	after, err := l.inputBytes(candidate, defs)
	if err != nil {
		return -1, err
	}
	afterTokens, afterBasis := l.tokenEstimate(candidate, after, l.Checkpoint.LastSequence)
	if after >= before || after > target || (targetTokens > 0 && afterTokens > targetTokens) {
		return -1, budgetError("compaction did not produce a smaller request within the input budget")
	}
	// All events for this attempt share its identity and source range. Enrich a
	// copy so a failed save still reports an attempt without a committed view.
	record := *attemptRecord
	record.Status = "committed"
	record.Summary, record.Quotes, record.View = text, quotes, candidate
	record.AfterBytes, record.AfterTokensEstimate = after, afterTokens
	record.TokenBasis = basis + " -> " + afterBasis
	if cut < len(body) {
		record.FirstKeptSequence = body[cut].Sequence
	}
	// Write the complete candidate to the durable event stream before the
	// checkpoint advances. The session checkpoint is the commit authority;
	// a crash after its save cannot erase this record on the next compaction.
	prepared := record
	prepared.Status = "prepared"
	l.emit(Event{Type: "context_compaction_prepared", Compaction: &prepared})
	oldHistory, oldCheckpoint := l.History, l.Checkpoint
	copyCheckpoint := *l.Checkpoint
	copyCheckpoint.SummaryRetries = 0
	copyCheckpoint.CompactionCount = record.ID
	metadata := record.CompactionCheckpoint
	copyCheckpoint.LastCompaction = &metadata
	l.History, l.Checkpoint = candidate, &copyCheckpoint
	if err := l.saveState(); err != nil {
		l.History, l.Checkpoint = oldHistory, oldCheckpoint
		return -1, err
	}
	l.emit(Event{Type: "context_compacted", Compaction: &record})
	return after, nil
}

// Count each complete message/tool group once. Joining nonempty JSON arrays
// replaces their adjacent brackets with a comma; escaping and wire metadata
// filtering therefore remain identical to marshaling the full suffix.
func recentHistoryStart(messages []Message, budget int) (int, error) {
	cut, size := len(messages), 2
	for cut > 0 {
		start := cut - 1
		if hasResults(messages[start]) {
			start--
			if start < 0 {
				return 0, errors.New("orphaned result during compaction")
			}
		}
		raw, err := json.Marshal(WireHistory(messages[start:cut]))
		if err != nil {
			return 0, err
		}
		next := size + len(raw) - 2
		if cut < len(messages) {
			next++
		}
		if next > budget {
			break
		}
		cut, size = start, next
	}
	return cut, nil
}
