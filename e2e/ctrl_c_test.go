package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Ctrl+C in a real terminal: it clears or cancels what is in front of the user and halts a
// running turn, but never quits hmj (Ctrl+D does that).

func TestCtrlCClearsTypedTextWithoutSendingIt(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	s.TypeSlowly("draft")
	s.Type("\x03")
	s.TypeSlowly("real\r")
	s.Expect("ok")
	s.ExpectPrompt()
	reqs := p.Requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0], "real") || strings.Contains(reqs[0], "draft") {
		t.Errorf("want one request with only the text typed after Ctrl+C, got %q", reqs)
	}
	s.Quit()
}

func TestCtrlCOnEmptyPromptKeepsRunning(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	headers := strings.Count(s.Screen(), "[model:")
	for range 3 {
		s.Type("\x03")
		time.Sleep(50 * time.Millisecond)
	}
	// Keys are handled in order, so once the typed "h" is on screen the Ctrl+Cs are done.
	s.Type("h")
	waitUntil(t, "the typed key is drawn", func() bool { return strings.Contains(s.Screen(), "> h") })
	if strings.Contains(s.Since(), "^C") {
		t.Errorf("Ctrl+C echoed ^C on an empty prompt")
	}
	if n := strings.Count(s.Screen(), "[model:"); n != headers {
		t.Errorf("Ctrl+C on an empty prompt drew %d new model headers", n-headers)
	}
	select {
	case <-s.exited:
		t.Fatalf("hmj exited (status %d) on Ctrl+C at an empty prompt", s.exitCode)
	default:
	}
	s.Type("i\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if n := len(p.Requests()); n != 1 {
		t.Errorf("want one request for the typed text, got %d", n)
	}
	s.Quit()
}

func TestCtrlCHaltsRunningToolAndKillsIt(t *testing.T) {
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

	s.Type("\x03")
	s.Expect("[halted by user (Ctrl+C)]")
	s.Expect("[command cancelled by user]")
	s.ExpectPrompt()
	waitUntil(t, "the tool process is gone", func() bool { return syscall.Kill(pid, 0) != nil })

	s.Type("still there?\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Quit()
}

func TestCtrlCHaltsWhileWaitingForTheFirstToken(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	// The reply never starts: the provider holds the request until hmj gives up on it.
	p.Queue(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	s.Type("slow question\r")
	waitUntil(t, "the request reaches the provider", func() bool { return len(p.Requests()) == 1 })
	s.Type("\x03")
	s.Expect("[halted by user (Ctrl+C)]")
	s.ExpectPrompt()
	s.Type("next\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Quit()
}

// Pressing Ctrl+C just as a turn ends, while hmj saves the session and draws the next
// prompt, must not quit hmj: the key lands on the next prompt instead.
func TestCtrlCAsTurnEndsDoesNotQuit(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	// Twenty turns: past the sixth, some turns also send a summary request, which the
	// provider's default "ok" answers like the turn itself.
	for i := range 20 {
		s.Type("question\r")
		s.Expect("ok")
		s.Type("\x03")
		s.ExpectPrompt()
		select {
		case <-s.exited:
			t.Fatalf("hmj exited (status %d) on Ctrl+C after turn %d", s.exitCode, i+1)
		default:
		}
	}
	s.Quit()
}

func TestCtrlCCancelsMenus(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(say("an answer"))
	s.Type("a question\r")
	s.Expect("an answer")
	s.ExpectPrompt()

	for _, menu := range []struct{ name, key, title string }{
		{"resume", "\x12", "select conversation"},
		{"models", "\x0f", "select model (type to filter"},
		{"history", "\x19", "browse turns"},
	} {
		s.Type(menu.key)
		s.Expect(menu.title)
		s.Type("\x03")
		s.ExpectPrompt()
	}
	// The history browser's second menu cancels too.
	s.Type("\x19")
	s.Expect("browse turns")
	s.Type("\r")
	s.Expect("choose action")
	s.Type("\x03")
	s.ExpectPrompt()

	// The slash menu returns to the same prompt in place, as Esc does, with the "/" removed.
	s.Type("/")
	s.Expect("commands (type to filter")
	s.Type("\x03")

	// Nothing was switched or sent: the next prompt goes to the same model as one request.
	s.TypeSlowly("after menus\r")
	s.Expect("ok")
	s.ExpectPrompt()
	reqs := p.Requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1], `"model":"model-a"`) || !strings.Contains(reqs[1], `"after menus"`) {
		t.Errorf("menus cancelled by Ctrl+C changed state; requests: %q", reqs)
	}
	s.Quit()
}

func TestCtrlCCancelsResumeRenameAndDelete(t *testing.T) {
	t.Parallel()
	_, s := startWithTwoConversations(t)
	s.Type("\x12") // Ctrl+R
	s.Expect("select conversation")
	s.Type("\x05") // Ctrl+E
	s.Expect("rename to:")
	s.TypeSlowly("new-name")
	s.Type("\x03")
	s.Expect("rename cancelled")
	s.Type("\x04") // Ctrl+D
	s.Expect("[y/N]")
	s.Type("\x03")
	s.Expect("delete cancelled")
	s.Type("\x03")
	s.ExpectPrompt()

	s.Type("\x12")
	s.Expect("select conversation")
	s.Expect("2/2 matches")
	if strings.Contains(s.Screen(), "new-name (") {
		t.Errorf("cancelled rename was applied")
	}
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
}

// interruptKey returns the key the terminal turns into SIGINT (0 when none). On Linux the
// master side of a pseudo terminal reports the settings hmj applied to its side.
func interruptKey(t *testing.T, s *session) byte {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(s.master.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Cc[unix.VINTR]
}

// hmj turns off the terminal's Ctrl+C interrupt while it runs, so the key cannot quit it
// between prompts, and must turn it back on at exit or Ctrl+C stops working in the shell.
func TestCtrlCInterruptIsRestoredOnExit(t *testing.T) {
	t.Parallel()
	t.Run("Ctrl+D exit", func(t *testing.T) {
		t.Parallel()
		_, s := start(t, newProvider(t))
		s.Quit()
		if k := interruptKey(t, s); k != 0x03 {
			t.Errorf("interrupt key after exit = %#x, want Ctrl+C (0x03)", k)
		}
	})
	t.Run("signal exit", func(t *testing.T) {
		t.Parallel()
		_, s := start(t, newProvider(t))
		s.Signal(syscall.SIGTERM)
		s.Wait()
		if k := interruptKey(t, s); k != 0x03 {
			t.Errorf("interrupt key after exit = %#x, want Ctrl+C (0x03)", k)
		}
	})
	t.Run("accessibility mode keeps it", func(t *testing.T) {
		// Its prompt uses the terminal's own line editing, where Ctrl+C keeps its usual meaning.
		t.Parallel()
		_, s := start(t, newProvider(t), "--accessible")
		if k := interruptKey(t, s); k != 0x03 {
			t.Errorf("interrupt key at the accessible prompt = %#x, want Ctrl+C (0x03)", k)
		}
		s.Quit()
	})
}
