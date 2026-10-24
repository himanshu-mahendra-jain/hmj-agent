package e2e

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// start writes a config for p and launches hmj, waiting for the first prompt.
func start(t *testing.T, p *provider, args ...string) (*env, *session) {
	t.Helper()
	e := newEnv(t)
	e.WriteConfig(p.config("model-a", "model-b"))
	s := e.Start(args...)
	s.ExpectPrompt()
	return e, s
}

func TestStartupAndCtrlDExit(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e, s := start(t, p)
	if !strings.Contains(s.Screen(), "HMJ Agent") || !strings.Contains(s.Screen(), "[model: mock/model-a · effort: default]") {
		t.Errorf("banner or model line missing")
	}
	s.Quit()
	if !strings.HasSuffix(s.Screen(), "\n") {
		t.Errorf("output must end with a newline so the shell prompt starts on its own line")
	}
	if _, err := os.Stat(conversationFile(e.Sessions, "1")); err != nil {
		t.Errorf("session file not saved on exit: %v", err)
	}
}

func TestSlashMenu(t *testing.T) {
	t.Parallel()
	t.Run("typed slowly", func(t *testing.T) {
		t.Parallel()
		_, s := start(t, newProvider(t))
		s.Type("/")
		s.Expect("commands (type to filter")
		s.TypeSlowly("short")
		s.Expect("/shortcuts  Display")
		s.Type("\r")
		s.Expect("HMJ Agent — Shortcuts & Commands")
		s.ExpectPrompt()
		s.Quit()
	})
	t.Run("typed in one burst", func(t *testing.T) {
		// Regression: the menu flushed the keys after "/" that arrived before it drew.
		t.Parallel()
		_, s := start(t, newProvider(t))
		s.Type("/shortcuts\r")
		s.Expect("HMJ Agent — Shortcuts & Commands")
		s.ExpectPrompt()
		s.Quit()
	})
	t.Run("Esc cancels and the prompt keeps working", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		_, s := start(t, p)
		s.Type("/")
		s.Expect("commands (type to filter")
		s.PressEsc()
		s.TypeSlowly("hello\r")
		s.Expect("ok")
		s.ExpectPrompt()
		if req := p.LastRequest(); !strings.Contains(req, `"hello"`) || strings.Contains(req, "/hello") {
			t.Errorf("prompt after a cancelled menu should be exactly hello: %s", req)
		}
		s.Quit()
	})
}

func TestFileToolsTurn(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	e.WriteFile("notes.txt", "alpha\nbeta\ngamma\n")
	e.WriteFile("main.go", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hi\") }\n")
	p.Queue(
		callTool("list_files", map[string]any{"path": "."}),
		callTool("read_file", map[string]any{"path": "notes.txt"}),
		callTool("edit_file", map[string]any{"path": "notes.txt", "old_string": "beta", "new_string": "BETA"}),
		callTool("write_file", map[string]any{"path": "out/summary.txt", "content": "done\n"}),
		callTool("run_command", map[string]any{"command": "echo from-shell-$((6*7))"}),
		say("All steps completed."),
	)

	s := e.Start()
	s.ExpectPrompt()
	s.Type("do the steps\r")
	s.Expect("list_files: . · 2 files · ")
	s.Expect("read_file: notes.txt · 3 lines · ")
	s.Expect("edit_file: notes.txt")
	s.Expect("+ BETA")
	s.Expect("write_file: out/summary.txt")
	s.Expect("run_command: echo from-shell")
	s.Expect("from-shell-42")
	s.Expect("All steps completed.")
	s.Expect("[tokens:")
	s.ExpectPrompt()
	s.Quit()

	if got := e.ReadFile("notes.txt"); got != "alpha\nBETA\ngamma\n" {
		t.Errorf("notes.txt = %q", got)
	}
	if got := e.ReadFile("out/summary.txt"); got != "done\n" {
		t.Errorf("summary.txt = %q", got)
	}
	// Each tool result went back to the model in the following request.
	reqs := p.Requests()
	if len(reqs) != 6 {
		t.Fatalf("got %d requests, want 6 (one per step)", len(reqs))
	}
	if !strings.Contains(reqs[2], "beta") || !strings.Contains(reqs[5], "from-shell-42") {
		t.Errorf("tool output missing from follow-up requests")
	}
	// The edited file was backed up, and every tool run was audited.
	backups, err := os.ReadDir(filepath.Join(filepath.Dir(e.Config), "backups"))
	if err != nil || len(backups) == 0 {
		t.Errorf("no backup of the edited file: %v", err)
	}
	audit, err := os.ReadFile(e.AuditLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"list_files", "read_file", "edit_file", "write_file", "run_command"} {
		if !strings.Contains(string(audit), `"tool":"`+tool+`"`) {
			t.Errorf("audit log has no %s entry", tool)
		}
	}
	// The session file holds the whole turn.
	data, err := os.ReadFile(conversationFile(e.Sessions, "1"))
	if err != nil || !jsontext.Value(data).IsValid() || !strings.Contains(string(data), "All steps completed.") {
		t.Errorf("session file missing or incomplete: %v", err)
	}
}

func TestEscHaltsRunningToolAndKillsIt(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config())
	pidFile := filepath.Join(e.Workspace, "child.pid")
	p.Queue(callTool("run_command", map[string]any{"command": "echo $$ > child.pid; exec sleep 60"}))

	s := e.Start()
	s.ExpectPrompt()
	s.Type("run the sleeper\r")
	s.Expect("run_command: echo")
	var pid int
	waitUntil(t, "the tool writes its pid", func() bool {
		data, err := os.ReadFile(pidFile)
		return err == nil && strings.HasSuffix(string(data), "\n") && json.Unmarshal([]byte(strings.TrimSpace(string(data))), &pid) == nil
	})
	defer syscall.Kill(pid, syscall.SIGKILL) // never leak the sleeper if an assertion fails

	s.PressEsc()
	// The notice comes after what the halted tool printed, just before the footer.
	s.Expect("[command cancelled by user]")
	s.Expect("[halted by user (Esc)]")
	s.Expect("[tokens:")
	s.ExpectPrompt()
	waitUntil(t, "the tool process is gone", func() bool { return syscall.Kill(pid, 0) != nil })

	// The agent is usable again after a halt.
	s.Type("still there?\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Quit()
}

func TestCtrlCHaltsStreamingReply(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(streamForever("tick", 20*time.Millisecond))
	s.Type("talk forever\r")
	s.Expect("tick\n  tick\n  tick") // each line after the 2-column margin
	s.Type("\x03")
	s.Expect("[halted by user (Ctrl+C)]")
	s.ExpectPrompt()
	// Ctrl+C halts the turn, not the program.
	s.Type("next\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Quit()
}

func TestSignalsExitWithConventionalStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sig  syscall.Signal
		want int
	}{
		{syscall.SIGHUP, 129},
		{syscall.SIGINT, 130},
		{syscall.SIGQUIT, 131},
		{syscall.SIGTERM, 143},
	} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			t.Parallel()
			e, s := start(t, newProvider(t))
			s.Signal(tc.sig)
			if got := s.Wait(); got != tc.want {
				t.Errorf("exit status = %d, want %d (128+%d)", got, tc.want, int(tc.sig))
			}
			if _, err := os.Stat(conversationFile(e.Sessions, "1")); err != nil {
				t.Errorf("session not saved: %v", err)
			}
		})
	}
}

func TestModelSwitchIsStickyAcrossRestarts(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e, s := start(t, p)
	s.Type("\x0f") // Ctrl+O
	s.Expect("select model (type to filter")
	s.TypeSlowly("model-b")
	s.Type("\r")
	s.Expect("model: mock/model-b")
	s.ExpectPrompt()
	s.Type("hi\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, `"model":"model-b"`) {
		t.Errorf("request did not use the selected model: %s", req)
	}
	s.Quit()

	s = e.Start()
	s.Expect("resumed conversation")
	s.Expect("[model: mock/model-b · effort: default]")
	s.Quit()
}

func TestConversationManager(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(say("first answer"), say("second answer"))
	s.Type("first chat\r")
	s.Expect("first answer")
	s.ExpectPrompt()
	s.Type("\x0e") // Ctrl+N
	s.Expect("created conversation")
	s.ExpectPrompt()
	s.Type("second chat\r")
	s.Expect("second answer")
	s.ExpectPrompt()

	// Rename the active conversation.
	s.Type("\x12") // Ctrl+R
	s.Expect("select conversation")
	s.Expect("second chat")
	s.Type("\x05") // Ctrl+E
	s.Expect("rename to:")
	s.TypeSlowly("renamed-chat\r")
	s.Expect(`renamed to "renamed-chat"`)
	s.PressEsc()
	s.ExpectPrompt()

	// Switch to the first one; its transcript is replayed.
	s.Type("\x12")
	s.Expect("renamed-chat")
	s.Type("\x1b[A") // Up
	s.Type("\r")
	s.Expect("resumed conversation")
	s.Expect("first answer")
	s.ExpectPrompt()

	// Delete the other one, confirming with y.
	s.Type("\x12")
	s.Expect("select conversation")
	s.Type("\x1b[B") // Down to renamed-chat
	s.Type("\x04")   // Ctrl+D
	s.Expect("[y/N]")
	s.Type("y")
	s.Expect("deleted conversation")
	s.PressEsc()
	s.ExpectPrompt()

	s.Type("\x12")
	s.Expect("select conversation")
	s.ExpectRegexp(`1/1 matches|1 matches`)
	if strings.Contains(s.Since(), "renamed-chat") {
		t.Errorf("deleted conversation is still listed")
	}
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
}

func TestTurnHistoryBrowser(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(say("the answer is 42"))
	s.Type("what is the answer\r")
	s.Expect("the answer is 42")
	s.ExpectPrompt()
	s.Type("\x19") // Ctrl+Y
	s.Expect("browse turns")
	s.Expect("what is the answer")
	s.Type("\r")
	s.Expect("choose action")
	s.TypeSlowly("view")
	s.Expect("View full turn in terminal")
	s.Type("\r")
	s.Expect("the answer is 42")
	s.ExpectPrompt()
	s.Quit()
}

func TestPromptLineEditing(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	// "world", Ctrl+A, "hello ", Ctrl+E, " junk", Ctrl+W deletes "junk".
	s.TypeSlowly("world")
	s.Type("\x01")
	s.TypeSlowly("hello ")
	s.Type("\x05")
	s.TypeSlowly(" junk")
	s.Type("\x17")
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, "hello world") || strings.Contains(req, "junk") {
		t.Errorf("edited prompt not sent as typed: %s", req)
	}
	// Ctrl+U clears the line.
	s.TypeSlowly("discard me")
	s.Type("\x15")
	s.TypeSlowly("kept\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); strings.Contains(req, "discard me") || !strings.Contains(req, "kept") {
		t.Errorf("Ctrl+U did not clear the line: %s", req)
	}
	// Word keys and Ctrl+K, step by step.
	s.TypeSlowly("cat src/main.go")
	s.Type("\x1b\x7f")  // Alt+Backspace: "cat src/main."
	s.Type("\x1b[1;5D") // Ctrl+Left: cursor before "main"
	s.Type("\x1bd")     // Alt+D: "cat src/."
	s.Type("\x1b[1;5C") // Ctrl+Right: no word follows, so the cursor goes to the end
	s.TypeSlowly("txt end")
	s.Type("\x1b[1;5D\x1b[1;5D") // Ctrl+Left twice: cursor before "txt"
	s.Type("\x0b")               // Ctrl+K: "cat src/."
	s.TypeSlowly("md\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, `"cat src/.md"`) {
		t.Errorf("word keys and Ctrl+K edited the prompt wrongly: %s", req)
	}
	s.Quit()
}

func TestBracketedPasteIsOnePrompt(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("\x1b[200~line one\nline two\nline three\x1b[201~")
	s.Expect("line three")
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	reqs := p.Requests()
	if len(reqs) != 1 {
		t.Fatalf("paste was split into %d requests, want 1", len(reqs))
	}
	if !strings.Contains(reqs[0], `line one\nline two\nline three`) {
		t.Errorf("pasted lines not sent as one prompt: %s", reqs[0])
	}
	s.Quit()
}

func TestPromptHistoryRecallAndDraft(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("first prompt\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Type("\x1b[A") // Up recalls it
	s.Expect("first prompt")
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if n := strings.Count(strings.Join(p.Requests(), ""), "first prompt"); n < 2 {
		t.Errorf("Up did not recall the previous prompt")
	}
	// A draft survives a trip through history.
	s.TypeSlowly("my draft")
	s.Type("\x1b[A")
	s.Expect("first prompt")
	s.Type("\x1b[B")
	s.Expect("my draft")
	s.Type("\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, "my draft") {
		t.Errorf("draft not restored after Up/Down: %s", req)
	}
	s.Quit()
}

func TestNewConversationKeepsEarlierOne(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.Type("hello\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Type("\x0e")
	s.Expect("HMJ Agent") // banner after the screen is cleared
	s.Expect("created conversation")
	s.ExpectPrompt()
	s.Type("\x12")
	s.Expect("select conversation")
	s.Expect("2/2 matches") // the earlier conversation is kept
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
}

func TestHealth(t *testing.T) {
	t.Parallel()
	t.Run("command", func(t *testing.T) {
		t.Parallel()
		_, s := start(t, newProvider(t))
		s.Type("/health\r")
		s.Expect("Provider & Model Health Check")
		s.Expect("2/2 models healthy")
		s.ExpectPrompt()
		s.Quit()
	})
	t.Run("flag", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		bad := newProvider(t)
		bad.Queue(httpError(402, `{"error":{"message":"out of credits"}}`))
		e.WriteConfig(bad.config("model-a"))
		s := e.Start("--health")
		s.Expect("HTTP 402")
		s.Expect("0/1 models healthy")
		s.Wait()
	})
}

func TestAccessibleMode(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p, "--accessible")
	s.Type("/models\r")
	s.Expect("select model")
	s.Expect("2. mock/model-b")
	s.Type("2\r")
	s.Expect("[OK] model: mock/model-b")
	s.ExpectPrompt()
	// A line ending in \ continues on the next line.
	s.Type("first half \\\r")
	s.Type("second half\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, "first half") || !strings.Contains(req, "second half") {
		t.Errorf("continued line not sent as one prompt: %s", req)
	}
	s.Quit()
	// Output is append-only: no cursor movement, erasing or redraws.
	if m := regexp.MustCompile(`\x1b\[[0-9;]*[ABCDEFGHJKST]`).FindString(s.Raw()); m != "" {
		t.Errorf("accessible mode wrote cursor control sequence %q", m)
	}
}

func TestNoColor(t *testing.T) {
	t.Parallel()
	_, s := start(t, newProvider(t), "--no-color")
	s.Type("hi\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Quit()
	if m := regexp.MustCompile(`\x1b\[[0-9;]*m`).FindString(s.Raw()); m != "" {
		t.Errorf("--no-color wrote color sequence %q", m)
	}
}

func TestStrictRejectsReadableKey(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(`{"providers":{"mock":{"baseUrl":"` + p.URL() + `","api":"responses","apiKey":"sk-literal-key","models":[{"id":"m"}]}}}`)
	if err := os.Chmod(e.Config, 0o644); err != nil {
		t.Fatal(err)
	}
	s := e.Start("--strict")
	s.Expect("insecure config rejected by --strict")
	if code := s.Wait(); code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	if strings.Contains(s.Screen(), "sk-literal-key") {
		t.Errorf("the API key was printed")
	}
}

func TestConfigHotReload(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e, s := start(t, p)
	e.WriteConfig(p.config("model-a", "model-b", "model-new"))
	s.Type("\x0f")
	s.Expect("select model")
	s.Expect("mock/model-new")
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
}

func TestProviderErrors(t *testing.T) {
	t.Parallel()
	t.Run("client error fails fast", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		_, s := start(t, p)
		p.Queue(httpError(402, `{"error":{"message":"You're out of credits"}}`))
		s.Type("hi\r")
		s.Expect("HTTP 402")
		s.Expect("out of credits")
		s.ExpectPrompt()
		if n := len(p.Requests()); n != 1 {
			t.Errorf("402 was retried: %d requests", n)
		}
		s.Quit()
	})
	t.Run("error body cannot drive the terminal", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		_, s := start(t, p)
		// A proxy's plain-text answer carries the bytes themselves, not JSON's \u001b.
		p.Queue(httpError(402, "out of credits\x1b]52;c;cHduZWQ=\a\x1b]0;pwned\a\x1b[2J"))
		s.Type("hi\r")
		s.Expect("out of credits")
		s.ExpectPrompt()
		for _, seq := range []string{"\x1b]52;", "\x1b]0;pwned", "\x1b[2J"} {
			if strings.Contains(s.Raw(), seq) {
				t.Errorf("the error body's %q reached the terminal", seq)
			}
		}
		s.Quit()
	})
	t.Run("server error is retried", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		_, s := start(t, p)
		p.Queue(httpError(503, `{"error":{"message":"busy"}}`), say("recovered"))
		s.Type("hi\r")
		s.Expect("recovered")
		s.ExpectPrompt()
		if n := len(p.Requests()); n != 2 {
			t.Errorf("got %d requests, want 2 (one retry)", n)
		}
		s.Quit()
	})
}

func TestFetchURL(t *testing.T) {
	t.Parallel()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><head><title>T</title><script>var hidden=1;</script></head>`+
			`<body><h1>Local Test Page</h1><p>The secret word is <b>pineapple</b>.</p></body></html>`)
	}))
	defer site.Close()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(callTool("fetch_url", map[string]any{"url": site.URL}), say("fetched"))
	s.Type("fetch it\r")
	s.Expect("fetch_url: " + site.URL + " · ") // the page's size and lines, not its text
	s.Expect(" B, 5 lines · ")
	s.Expect("fetched")
	s.ExpectPrompt()
	s.Quit()
	if req := p.Requests()[1]; !strings.Contains(req, "pineapple") || strings.Contains(req, "var hidden") {
		t.Errorf("page text not sent back as plain text: %s", req)
	}
}
