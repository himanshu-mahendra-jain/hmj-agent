package e2e

import (
	"os"
	"strings"
	"testing"
)

// TestTwoInstancesInOneProject runs two hmj processes in the same workspace: the second starts
// its own conversation, /resume there marks the first one's as open in another hmj and refuses
// to open it, and both conversations keep their turns once both have saved.
func TestTwoInstancesInOneProject(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config("model-a"))

	first := e.Start("--accessible")
	first.ExpectPrompt()
	first.Type("from the first\r")
	first.Expect("ok")
	first.ExpectPrompt()

	second := e.Start("--accessible")
	second.ExpectPrompt()
	second.Type("from the second\r")
	second.Expect("ok")
	second.ExpectPrompt()
	second.Type("/resume\r")
	second.Expect("from the first (1 user message")
	second.Expect("[open in another hmj]")
	second.Type("1\r")
	second.Expect("/resume failed: conversation 1 is open in another hmj (pid ")
	second.ExpectPrompt()

	first.Type("more from the first\r")
	first.Expect("ok")
	first.ExpectPrompt()
	second.Quit()
	first.Quit()

	for id, want := range map[string][]string{"1": {"from the first", "more from the first"}, "2": {"from the second"}} {
		data, err := os.ReadFile(conversationFile(e.Sessions, id))
		if err != nil {
			t.Fatal(err)
		}
		for _, prompt := range want {
			if !strings.Contains(string(data), prompt) {
				t.Errorf("conversation %s lacks %q: %s", id, prompt, data)
			}
		}
	}
}
