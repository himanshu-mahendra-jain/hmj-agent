package tools

import "context"

type scratchDirKey struct{}

// WithScratchDir returns a copy of ctx that carries the conversation's scratch directory, which
// run_command gives its commands as $HMJ_SCRATCH.
func WithScratchDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, scratchDirKey{}, dir)
}

// scratchDirFrom returns the scratch directory ctx carries, or "".
func scratchDirFrom(ctx context.Context) string {
	dir, _ := ctx.Value(scratchDirKey{}).(string)
	return dir
}
