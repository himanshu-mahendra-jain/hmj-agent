package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
)

// runEnv is the isolated files of one run(): config, session file and workspace.
type runEnv struct {
	config, sessions string
	requests         *atomic.Int32 // model requests the mock provider received
}

// newRunEnv points hmj at a fresh HOME, config, session file and workspace, with a mock
// Responses provider "prov1" offering model m1 that answers every request with "ok". The
// audit log and diagnostic log are off unless a test turns them on.
func newRunEnv(t *testing.T) runEnv {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	e := runEnv{config: filepath.Join(root, "config.json"), sessions: filepath.Join(root, "sessions.json"), requests: &requests}
	cfg := fmt.Sprintf(`{"providers":{"prov1":{"baseUrl":%q,"api":"responses","apiKey":"k","models":[{"id":"m1"}]}}}`, srv.URL)
	if err := os.WriteFile(e.config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("HMJ_CONFIG", e.config)
	t.Setenv("HMJ_SESSIONS", e.sessions)
	t.Setenv("HMJ_AUDIT_LOG", "off")
	t.Setenv("HMJ_LOG", "")
	t.Setenv("ACCESSIBILITY", "")
	t.Setenv("HIGH_CONTRAST", "")
	t.Chdir(t.TempDir())
	t.Cleanup(func() { theme.SetAccessibility(false); theme.SetHighContrast(false) })
	return e
}

// runCapturing runs hmj with args on input and returns its exit code and everything written
// to stderr, through the stderr argument and os.Stderr alike.
func runCapturing(t *testing.T, args []string, input io.Reader) (int, string) {
	t.Helper()
	var code int
	var errBuf bytes.Buffer
	osErr := captureStderr(t, func() { code = run(args, input, io.Discard, &errBuf) })
	return code, errBuf.String() + osErr
}

// regularFile creates an empty file and returns its path, for paths that must fail because
// a directory is expected there.
func regularFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunWarnsWhenLogsCannotBeOpened checks that a diagnostic log or audit log that cannot be
// created is a warning, not a reason to refuse to start.
func TestRunWarnsWhenLogsCannotBeOpened(t *testing.T) {
	newRunEnv(t)
	blocker := regularFile(t)
	t.Setenv("HMJ_LOG", filepath.Join(blocker, "hmj.log"))
	t.Setenv("HMJ_AUDIT_LOG", filepath.Join(blocker, "audit.log"))

	code, stderr := runCapturing(t, nil, strings.NewReader(""))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr %q", code, stderr)
	}
	for _, want := range []string{"[warning: diagnostic log disabled:]", "[warning: audit log disabled:]"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// TestRunReplacesAModelNoLongerConfigured checks that a resumed conversation whose model was
// removed from the config switches to the config's default model, and that is saved.
func TestRunReplacesAModelNoLongerConfigured(t *testing.T) {
	e := newRunEnv(t)
	saved := `{"activeId":"1","order":["1"],"counter":1,"defaultModel":{"provider":"gone","model":"old"},
		"conversations":{"1":{"id":"1","name":"kept","model":{"provider":"gone","model":"old"},
		"messages":[{"role":"system","content":"s"},{"role":"user","content":"earlier prompt"},{"role":"assistant","content":"earlier answer"}]}}}`
	if err := os.WriteFile(e.sessions, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, stderr := runCapturing(t, nil, strings.NewReader("")); code != 0 {
		t.Fatalf("exit code = %d; stderr %q", code, stderr)
	}
	sm := session.LoadSessionManager(e.sessions, config.ModelRef{})
	if got := sm.Active().Model; got != (config.ModelRef{Provider: "prov1", Model: "m1"}) {
		t.Errorf("model after restart = %+v, want prov1/m1", got)
	}
	if sm.Active().UserMessageCount() != 1 {
		t.Error("the conversation's messages were lost")
	}
}

// TestRunStartupConfigChecks checks the config checks made at startup: --strict refuses a
// config with security warnings, and a key read from an unset environment variable is
// warned about without stopping the run.
func TestRunStartupConfigChecks(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		apiKey   string
		mode     os.FileMode
		wantCode int
		want     string
	}{
		{"--strict rejects a world-readable literal key", []string{"--strict"}, "literal-secret-key", 0o644, 1, "insecure config rejected by --strict"},
		{"unset key variable", nil, "$HMJ_TEST_UNSET_KEY_VAR", 0o600, 0, "config warning: provider prov1: apiKey refers to $HMJ_TEST_UNSET_KEY_VAR, which is not set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunEnv(t)
			t.Setenv("HMJ_TEST_UNSET_KEY_VAR", "")
			cfg := fmt.Sprintf(`{"providers":{"prov1":{"baseUrl":"https://127.0.0.1:1","api":"responses","apiKey":%q,"models":[{"id":"m1"}]}}}`, tc.apiKey)
			if err := os.WriteFile(e.config, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(e.config, tc.mode); err != nil {
				t.Fatal(err)
			}
			code, stderr := runCapturing(t, tc.args, strings.NewReader(""))
			if code != tc.wantCode || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q; want exit %d and %q", code, stderr, tc.wantCode, tc.want)
			}
		})
	}
}

// TestRunIgnoresEmptyInputAndReportsClosedInput checks that an empty line sends nothing to
// the model, and that a read error other than end of input is reported before exiting.
func TestRunIgnoresEmptyInputAndReportsClosedInput(t *testing.T) {
	e := newRunEnv(t)
	input := io.MultiReader(strings.NewReader("\n\n"), iotest.ErrReader(errors.New("device unplugged")))
	code, stderr := runCapturing(t, nil, input)
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if n := e.requests.Load(); n != 0 {
		t.Errorf("empty lines sent %d model requests, want 0", n)
	}
	if !strings.Contains(stderr, "[input closed:] device unplugged") {
		t.Errorf("stderr = %q, want the input error", stderr)
	}
}

// TestRunReportsFailedCommands checks that a command that fails names itself in the error,
// for each slash command that can fail.
func TestRunReportsFailedCommands(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		input string
		setup func(t *testing.T, e runEnv)
		want  string
	}{
		{name: "/new cannot save", input: "/new\n", want: "/new failed:", setup: func(t *testing.T, e runEnv) {
			t.Setenv("HMJ_SESSIONS", filepath.Join(regularFile(t), "sessions.json"))
		}},
		{name: "/resume cannot save", args: []string{"--accessible"}, input: "/resume\n1\n", want: "/resume failed:", setup: func(t *testing.T, e runEnv) {
			t.Setenv("HMJ_SESSIONS", filepath.Join(regularFile(t), "sessions.json"))
		}},
		{name: "/history cannot copy", args: []string{"--accessible"}, input: "hello\n/history\n1\n1\n", want: "/history failed: clipboard unavailable", setup: func(t *testing.T, e runEnv) {
			t.Setenv("PATH", t.TempDir()) // no wl-copy, xclip or xsel; stdout is not a terminal
		}},
		{name: "/models cannot save", args: []string{"--accessible"}, input: "/models\n1\n", want: "/models failed:", setup: func(t *testing.T, e runEnv) {
			t.Setenv("HMJ_SESSIONS", filepath.Join(regularFile(t), "sessions.json"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunEnv(t)
			if tc.setup != nil {
				tc.setup(t, e)
			}
			code, stderr := runCapturing(t, tc.args, strings.NewReader(tc.input))
			if code != 0 {
				t.Fatalf("exit code = %d", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// TestMenusClosedByEndOfInputAreNotFailures checks that input ending (Ctrl+D) in an
// accessible menu cancels the command quietly instead of reporting it as failed.
func TestMenusClosedByEndOfInputAreNotFailures(t *testing.T) {
	for _, input := range []string{"/resume\n", "/models\n", "hello\n/history\n"} {
		t.Run(strings.TrimSpace(input), func(t *testing.T) {
			newRunEnv(t)
			code, stderr := runCapturing(t, []string{"--accessible"}, strings.NewReader(input))
			if code != 0 || strings.Contains(stderr, "failed") {
				t.Errorf("exit code %d, stderr %q; want a clean exit without a failure", code, stderr)
			}
		})
	}
}

// TestPromptModelFallsBackWhenModelRemoved checks that when the active conversation's model
// disappears from the config while hmj runs, the next prompt uses the default model and new
// conversations inherit it.
func TestPromptModelFallsBackWhenModelRemoved(t *testing.T) {
	a := newReloadApp(t, forcedFlags{})
	defer a.cancel()
	a.sm = session.NewSessionManager(config.ModelRef{Provider: "gone", Model: "old"})

	var conv *session.Conversation
	captureStderr(t, func() { conv = a.promptModel() })
	if conv.Model != a.modelRef || a.sm.DefaultModel != a.modelRef {
		t.Errorf("model = %+v, default = %+v; want both %+v", conv.Model, a.sm.DefaultModel, a.modelRef)
	}
}

// TestSignalExitCodeForOtherSignals checks that a signal that is not a syscall.Signal exits
// like an interrupt.
func TestSignalExitCodeForOtherSignals(t *testing.T) {
	if got := signalExitCode(fakeSignal{}); got != 130 {
		t.Errorf("signalExitCode(fake) = %d, want 130", got)
	}
}

type fakeSignal struct{}

func (fakeSignal) String() string { return "fake" }
func (fakeSignal) Signal()        {}

// TestRunHealthAnnouncesDetectedProtocol checks that --health on a provider without an
// "api" probes it, says which protocol it found, and saves it to the config.
func TestRunHealthAnnouncesDetectedProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "{}" { // the protocol probe
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"model is required"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"delta\":\"1\"}\n\n")
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := fmt.Sprintf(`{"providers":{"auto":{"baseUrl":%q,"apiKey":"k","models":[{"id":"m1"}]}}}`, srv.URL)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HMJ_CONFIG", path)

	var stdout, stderr bytes.Buffer
	if code := runHealth(&stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `[provider auto speaks the responses API; saved "api": "responses" to the config file]`) {
		t.Errorf("stderr = %q, want the detected protocol", stderr.String())
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"api": "responses"`) && !strings.Contains(string(data), `"api":"responses"`) {
		t.Errorf("config not updated: %s", data)
	}
}
