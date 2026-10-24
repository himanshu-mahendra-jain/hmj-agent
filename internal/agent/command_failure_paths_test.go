package agent

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
)

// unsavableManager returns a session manager with conversations named names whose session
// directory cannot be written: it lies under a regular file.
func unsavableManager(t *testing.T, names ...string) *session.SessionManager {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sm := session.LoadSessionManager(filepath.Join(blocker, "sessions"), config.ModelRef{Provider: "p", Model: "m1"})
	sm.Rename(sm.ActiveID, names[0])
	for _, n := range names[1:] {
		sm.New(n)
	}
	return sm
}

// twoModelConfig offers p/m1 and p/m2.
func twoModelConfig() config.Config {
	return config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: "http://127.0.0.1:1", Models: []config.Model{{ID: "m1"}, {ID: "m2", Name: "Second"}}},
	}}
}

// lines returns a reader that yields input, as typed into a non-terminal stdin.
func lines(input string) *bufio.Reader { return bufio.NewReader(strings.NewReader(input)) }

// useAccessibility turns accessibility mode on for the rest of the test.
func useAccessibility(t *testing.T) {
	t.Helper()
	theme.SetAccessibility(true)
	t.Cleanup(func() { theme.SetAccessibility(false) })
}

// TestCommandsReportSaveFailures checks that every command that changes the conversations
// returns the session save failure instead of reporting success.
func TestCommandsReportSaveFailures(t *testing.T) {
	cases := []struct {
		name string
		run  func(sm *session.SessionManager) error
	}{
		{"new", func(sm *session.SessionManager) error { return HandleNew(sm) }},
		{"models", func(sm *session.SessionManager) error { return HandleModels(twoModelConfig(), sm, lines("2\n")) }},
		{"resume", func(sm *session.SessionManager) error { return HandleResume(config.Config{}, sm, lines("1\n")) }},
		{"rename", func(sm *session.SessionManager) error {
			var msgs []string
			_, err := resumeCallbacks(sm, &msgs).OnRename(resumeItem(sm, "first"), "renamed")
			if len(msgs) != 0 {
				t.Errorf("a failed rename was announced: %q", msgs)
			}
			return err
		}},
		{"delete", func(sm *session.SessionManager) error {
			var msgs []string
			err := resumeCallbacks(sm, &msgs).OnDelete(resumeItem(sm, "first"))
			if len(msgs) != 0 {
				t.Errorf("a failed delete was announced: %q", msgs)
			}
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := unsavableManager(t, "first", "second")
			var err error
			out := captureOutput(t, func() { err = tc.run(sm) })
			if err == nil {
				t.Fatalf("no error although the session file cannot be written; output %q", out)
			}
			if !strings.Contains(err.Error(), "not a directory") {
				t.Errorf("err = %v, want the filesystem error", err)
			}
			for _, success := range []string{"created conversation", "model:", "resumed conversation"} {
				if strings.Contains(out, success) {
					t.Errorf("success %q printed although saving failed: %q", success, out)
				}
			}
		})
	}
}

// resumeItem returns the resume selector item of the conversation named name.
func resumeItem(sm *session.SessionManager, name string) terminal.SelectItem {
	items, _ := buildResumeItems(sm.List(), sm)
	for _, it := range items {
		if strings.Contains(it.Text, "] "+name+" (") {
			return it
		}
	}
	return terminal.SelectItem{}
}

// TestSelectorsEndedByEOFCancel checks that when input ends (Ctrl+D) while an accessible
// menu waits for a choice, the command is cancelled: no error, and nothing changes.
func TestSelectorsEndedByEOFCancel(t *testing.T) {
	useAccessibility(t)
	sm := session.LoadSessionManager("", config.ModelRef{Provider: "p", Model: "m1"})
	sm.New("second")
	active := sm.ActiveID

	var errModels, errResume error
	captureOutput(t, func() {
		errModels = HandleModels(twoModelConfig(), sm, lines(""))
		errResume = HandleResume(config.Config{}, sm, lines(""))
	})
	if errModels != nil || errResume != nil {
		t.Errorf("errors = %v, %v; want both menus cancelled without an error", errModels, errResume)
	}
	if sm.ActiveID != active || sm.Active().Model.Model != "m1" {
		t.Errorf("state changed: active %s (was %s), model %s", sm.ActiveID, active, sm.Active().Model.Model)
	}
}

// TestHandleModelsWithoutModels checks the message for a config without models.
func TestHandleModelsWithoutModels(t *testing.T) {
	sm := session.LoadSessionManager("", config.ModelRef{})
	out := captureOutput(t, func() {
		if err := HandleModels(config.Config{}, sm, lines("1\n")); err != nil {
			t.Errorf("HandleModels: %v", err)
		}
	})
	if !strings.Contains(out, "no models configured") {
		t.Errorf("output = %q", out)
	}
}

// TestHandleModelsSelectsByProviderWithLeadingDigits checks that choosing a model whose
// provider name starts with digits (such as "2fast") selects that model, not the model at
// the position those digits name. Providers are listed by name, so 2fast/gpt is item 1 and
// acme/a1 is item 2.
func TestHandleModelsSelectsByProviderWithLeadingDigits(t *testing.T) {
	useAccessibility(t)
	cfg := config.Config{Providers: map[string]config.Provider{
		"acme":  {BaseURL: "http://127.0.0.1:1", Models: []config.Model{{ID: "a1"}}},
		"2fast": {BaseURL: "http://127.0.0.1:1", Models: []config.Model{{ID: "gpt"}}},
	}}
	sm := session.LoadSessionManager("", config.ModelRef{Provider: "acme", Model: "a1"})
	captureOutput(t, func() {
		if err := HandleModels(cfg, sm, lines("1\n")); err != nil {
			t.Fatalf("HandleModels: %v", err)
		}
	})
	if got := sm.Active().Model; got != (config.ModelRef{Provider: "2fast", Model: "gpt"}) {
		t.Errorf("active model = %s/%s, want 2fast/gpt", got.Provider, got.Model)
	}
}

// TestHandleHealthWithoutModels checks that a health check of a config without models
// says so instead of probing.
func TestHandleHealthWithoutModels(t *testing.T) {
	var out strings.Builder
	if err := HandleHealthTo(config.Config{}, chat.NewClient(config.Config{}), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no models configured") {
		t.Errorf("output = %q", out.String())
	}
}

// TestHandleModelsShowsEffort checks that each model in the /models list names the effort its
// requests carry.
func TestHandleModelsShowsEffort(t *testing.T) {
	useAccessibility(t)
	cfg := config.Config{Providers: map[string]config.Provider{
		"claude": {BaseURL: "http://127.0.0.1:1", API: config.APIAnthropic, Models: []config.Model{{ID: "opus", Effort: "high"}}},
		"local":  {BaseURL: "http://127.0.0.1:1", API: config.APICompletions, Models: []config.Model{{ID: "qwen", Effort: "low"}}},
	}}
	sm := session.LoadSessionManager("", config.ModelRef{Provider: "claude", Model: "opus"})
	out := captureOutput(t, func() {
		if err := HandleModels(cfg, sm, lines("\n")); err != nil {
			t.Fatalf("HandleModels: %v", err)
		}
	})
	for _, want := range []string{"claude/opus | effort: high", "local/qwen | effort: default (low not sent: Chat Completions)"} {
		if !strings.Contains(out, want) {
			t.Errorf("/models list lacks %q:\n%s", want, out)
		}
	}
}
