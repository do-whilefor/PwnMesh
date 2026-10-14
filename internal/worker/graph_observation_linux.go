//go:build linux

package worker

import "context"

type internalGraphReadKey struct{}

// Internal fragments are not model-visible observations. The logical reader
// records the assembled, validated result after all transport reads complete.
func withInternalGraphRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalGraphReadKey{}, true)
}

func internalGraphRead(ctx context.Context) bool {
	hidden, _ := ctx.Value(internalGraphReadKey{}).(bool)
	return hidden
}
