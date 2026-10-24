package tools

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestRunCommandGetsTheScratchDir checks that a command sees the conversation's scratch directory
// as $HMJ_SCRATCH, and no such variable without one.
func TestRunCommandGetsTheScratchDir(t *testing.T) {
	tool := &RunCommandTool{}
	args := `{"command":"echo \"s=${HMJ_SCRATCH-unset}\""}`
	out, err := tool.Execute(WithScratchDir(context.Background(), "/tmp/hmj scratch/7"), args)
	if err != nil || !strings.Contains(out, "s=/tmp/hmj scratch/7") {
		t.Errorf("with a scratch dir: %q, %v", out, err)
	}
	t.Setenv("HMJ_SCRATCH", "")
	os.Unsetenv("HMJ_SCRATCH")
	out, err = tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(out, "s=unset") {
		t.Errorf("without one: %q, %v", out, err)
	}
}
