package chat

import "context"

// toolProgress is told which tool call the model is writing and how many bytes of its
// arguments have arrived. A nil toolProgress ignores the reports.
type toolProgress func(name string, argBytes int)

func (p toolProgress) report(name string, argBytes int) {
	if p != nil && name != "" {
		p(name, argBytes)
	}
}

type toolProgressKey struct{}

// WithToolCallProgress returns ctx carrying fn, which a streamed reply calls while the model
// writes a tool call: with the tool's name and no bytes when the call starts, and with the size
// of its arguments so far as they arrive. It runs on the goroutine that calls StreamChat.
func WithToolCallProgress(ctx context.Context, fn func(name string, argBytes int)) context.Context {
	return context.WithValue(ctx, toolProgressKey{}, toolProgress(fn))
}

// toolProgressFrom returns the function WithToolCallProgress put in ctx, or nil.
func toolProgressFrom(ctx context.Context) toolProgress {
	p, _ := ctx.Value(toolProgressKey{}).(toolProgress)
	return p
}
