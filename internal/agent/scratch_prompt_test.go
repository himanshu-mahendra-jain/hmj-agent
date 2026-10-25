package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/v2/internal/session"
)

// TestFirstTurnNamesTheScratchDir checks that a saved conversation's first turn creates its
// scratch directory and names it in the system prompt, and that an unsaved one gets no such line.
func TestFirstTurnNamesTheScratchDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratch", "3")
	conv := &session.Conversation{ID: "3", ScratchDir: dir}
	(&Runner{}).beginTurn(conv, "hi")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("scratch directory not created: %v", err)
	}
	if got := conv.Messages[0].Content; conv.Messages[0].Role != "system" || !strings.HasSuffix(got, session.ScratchPrompt(dir)) {
		t.Errorf("system prompt = %q, want it to end with the scratch paragraph", got)
	}

	unsaved := &session.Conversation{ID: "4"}
	(&Runner{}).beginTurn(unsaved, "hi")
	if strings.Contains(unsaved.Messages[0].Content, "scratch directory") {
		t.Errorf("unsaved conversation's prompt names a scratch directory: %q", unsaved.Messages[0].Content)
	}
}
