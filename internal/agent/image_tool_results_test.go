package agent

import (
	"context"
	"errors"
	"testing"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/tools"
)

// pictureTool answers with a picture, or with err when set.
type pictureTool struct{ err error }

func (pictureTool) Name() string                    { return "picture_tool" }
func (pictureTool) Definition() chat.ToolDefinition { return chat.ToolDefinition{} }
func (pictureTool) Execute(context.Context, string) (string, error) {
	return "", errors.New("Execute called in place of ExecuteWithImages")
}
func (p pictureTool) ExecuteWithImages(context.Context, string) (tools.Result, error) {
	return tools.Result{Text: "a picture", Images: []chat.Image{{File: "x.png", MediaType: "image/png"}}}, p.err
}

// TestRunToolPassesImagesOn checks that a tool's pictures reach the tool message, and that a
// failed call carries only its error.
func TestRunToolPassesImagesOn(t *testing.T) {
	tc := chat.ToolCall{ID: "c1", Function: chat.ToolCallFunction{Name: "picture_tool", Arguments: "{}"}}
	r := &Runner{Tools: map[string]tools.Tool{"picture_tool": pictureTool{}}}
	var text string
	var images []chat.Image
	captureOutput(t, func() { text, images = r.runTool(context.Background(), tc, nil) })
	if text != "a picture" || len(images) != 1 || images[0].File != "x.png" {
		t.Errorf("got %q, %v; want the text and one picture", text, images)
	}

	r.Tools["picture_tool"] = pictureTool{err: errors.New("broken")}
	captureOutput(t, func() { text, images = r.runTool(context.Background(), tc, nil) })
	if text != "error: broken" || images != nil {
		t.Errorf("failed call: got %q, %v; want only the error", text, images)
	}
}
