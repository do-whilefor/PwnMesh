package agent

import "slices"

// Public mutators may run before Run (for example, recovery followed by a
// conclusion instruction). Detach the seed once, before the first mutation.
func (l *Loop) ownState() {
	if !l.stateOwned {
		l.History = cloneMessages(l.History)
		l.Checkpoint = cloneCheckpoint(l.Checkpoint)
		l.stateOwned = true
	}
}

func cloneUsage(usage *Usage) *Usage {
	if usage == nil {
		return nil
	}
	copy := *usage
	return &copy
}

func cloneMessage(message Message) Message {
	message.Content = slices.Clone(message.Content)
	for i := range message.Content {
		message.Content[i].Input = slices.Clone(message.Content[i].Input)
		message.Content[i].Content = slices.Clone(message.Content[i].Content)
	}
	message.Usage = cloneUsage(message.Usage)
	return message
}

func cloneMessages(messages []Message) []Message {
	copy := slices.Clone(messages)
	for i := range copy {
		copy[i] = cloneMessage(copy[i])
	}
	return copy
}

func cloneDefinitions(definitions []Definition) []Definition {
	copy := slices.Clone(definitions)
	for i := range copy {
		copy[i].Schema = slices.Clone(copy[i].Schema)
	}
	return copy
}

func cloneCheckpoint(checkpoint *ContextCheckpoint) *ContextCheckpoint {
	if checkpoint == nil {
		return nil
	}
	copy := *checkpoint
	if copy.LastCompaction != nil {
		last := *copy.LastCompaction
		last.Quotes = slices.Clone(last.Quotes)
		last.Usage = cloneUsage(last.Usage)
		copy.LastCompaction = &last
	}
	return &copy
}
