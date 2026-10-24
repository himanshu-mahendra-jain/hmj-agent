// Package e2e drives the real hmj binary inside a pseudo terminal, the way a person
// uses it: keys go in, the screen comes out. Unit tests swap os.Stdin for a PTY inside
// one package; these tests catch what only shows up across the whole program, such as
// raw-mode key handling between the prompt and its menus, signal exit codes, terminal
// restoration, session files and the audit log.
//
// Every test gets its own HOME, config, session file, audit log and workspace, and talks
// to a scripted mock provider (mock_provider_test.go), so the suite is hermetic and runs
// offline. live_provider_test.go adds an opt-in smoke test against a real provider.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// binPath is the hmj binary built once by TestMain for every test in the package.
var binPath string

// coverDir is HMJ_COVER_DIR: when set (see `make cover`), the binary is built with
// coverage instrumentation and every run writes its counters there through GOCOVERDIR,
// so what these tests exercise counts towards the merged coverage report. Counters are
// written on exit, so a run only counts when hmj exits by itself (Ctrl+D, /exit, a
// signal it handles) rather than being killed when the test ends.
var coverDir = os.Getenv("HMJ_COVER_DIR")

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hmj-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: temp dir:", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "hmj")
	args := []string{"build", "-trimpath", "-o", binPath}
	if coverDir != "" {
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=hmj-agent/...")
	}
	build := exec.Command("go", append(args, "hmj-agent")...)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: building hmj: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// expectTimeout bounds every wait for output. Mock replies are instant, so anything
// slower than this is a hang, not a slow machine.
const expectTimeout = 10 * time.Second

// env is the isolated filesystem one test runs hmj against.
type env struct {
	t         *testing.T
	Home      string // HOME for the child, so nothing touches the real ~/.hmj
	Workspace string // working directory of hmj
	Config    string // HMJ_CONFIG
	Sessions  string // HMJ_SESSIONS, the session directory
	AuditLog  string // HMJ_AUDIT_LOG
	Rows      int    // terminal size for Start; 40x120 unless changed
	Cols      int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{
		t:         t,
		Home:      filepath.Join(root, "home"),
		Workspace: filepath.Join(root, "ws"),
		Config:    filepath.Join(root, "hmj", "config.json"),
		Sessions:  filepath.Join(root, "hmj", "sessions"),
		AuditLog:  filepath.Join(root, "hmj", "audit.log"),
		Rows:      40,
		Cols:      120,
	}
	for _, d := range []string{e.Home, e.Workspace, filepath.Dir(e.Config)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// WriteConfig writes the config file with mode 0600.
func (e *env) WriteConfig(json string) {
	e.t.Helper()
	if err := os.WriteFile(e.Config, []byte(json), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// WriteFile writes a file relative to the workspace.
func (e *env) WriteFile(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.Workspace, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// ReadFile reads a file relative to the workspace, failing the test when it is missing.
func (e *env) ReadFile(rel string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.Workspace, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

// session is one running hmj process attached to a pseudo terminal.
type session struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File

	mu     sync.Mutex
	raw    []byte // everything hmj wrote, escape sequences included
	cursor int    // Expect searches the stripped screen from here on

	timeout time.Duration // how long Expect and Wait wait; expectTimeout unless changed

	rows, cols int            // terminal size at start
	resizes    []screenResize // later size changes, replayed by ScreenState

	readDone chan struct{}
	exited   chan struct{}
	exitCode int
}

// Start runs hmj with args in an e.Rows x e.Cols terminal (40x120 by default). The process is killed when the test ends.
func (e *env) Start(args ...string) *session {
	return e.StartWithEnv(nil, args...)
}

// StartWithEnv is Start with extra environment variables (KEY=value).
func (e *env) StartWithEnv(extra []string, args ...string) *session {
	t := e.t
	t.Helper()
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("opening a PTY: %v", err)
	}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(e.Rows), Col: uint16(e.Cols)}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binPath, args...)
	cmd.Dir = e.Workspace
	cmd.Env = append(cleanEnv(),
		"HOME="+e.Home,
		"TERM=xterm-256color",
		"HMJ_CONFIG="+e.Config,
		"HMJ_SESSIONS="+e.Sessions,
		"HMJ_AUDIT_LOG="+e.AuditLog,
	)
	if coverDir != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+coverDir)
	}
	cmd.Env = append(cmd.Env, extra...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close() // the child holds its own copy; reads on master end when it exits

	s := &session{t: t, cmd: cmd, master: master, timeout: expectTimeout, rows: e.Rows, cols: e.Cols, readDone: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer close(s.readDone)
		chunk := make([]byte, 8192)
		for {
			n, err := master.Read(chunk)
			s.mu.Lock()
			s.raw = append(s.raw, chunk[:n]...)
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		s.exitCode = 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			s.exitCode = exitErr.ExitCode()
		}
		close(s.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-s.exited:
		default:
			if coverDir != "" {
				// SIGTERM is a graceful exit, so the coverage counters get written.
				syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
				select {
				case <-s.exited:
				case <-time.After(2 * time.Second):
				}
			}
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-s.exited
		}
		master.Close()
		<-s.readDone
		if t.Failed() {
			t.Logf("output (last 3000 bytes, escapes stripped):\n%s", tail(s.Screen(), 3000))
			t.Logf("rendered %s", s.ScreenState().Dump())
		}
	})
	return s
}

// cleanEnv is the parent environment without HMJ_* and colour settings, provider credentials
// and the display variables, so a developer's own setup cannot leak into a test and copying
// from /history cannot reach the developer's real clipboard through wl-copy or xclip.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "HMJ_"), k == "ACCESSIBILITY", k == "HIGH_CONTRAST":
		case k == "NO_COLOR", k == "FORCE_COLOR", k == "CLICOLOR", k == "CLICOLOR_FORCE":
		case k == "DISPLAY", k == "WAYLAND_DISPLAY":
		case strings.HasSuffix(k, "_API_KEY"), strings.HasSuffix(k, "_TOKEN"):
		default:
			out = append(out, kv)
		}
	}
	return out
}

// Type sends keys exactly as given. Escape sequences such as "\x1b[A" (Up) are sent as one
// burst, which is how a terminal delivers them.
func (s *session) Type(keys string) {
	s.t.Helper()
	if _, err := s.master.WriteString(keys); err != nil {
		s.t.Fatalf("typing %q: %v", keys, err)
	}
}

// PressEsc sends a lone Esc and waits past the 25ms window in which a following byte
// would make it the start of an escape sequence (Alt+key) instead of the Esc key.
func (s *session) PressEsc() {
	s.t.Helper()
	s.Type("\x1b")
	time.Sleep(100 * time.Millisecond)
}

// TypeSlowly sends keys one at a time with a short pause, like a person typing.
func (s *session) TypeSlowly(keys string) {
	s.t.Helper()
	for _, r := range keys {
		s.Type(string(r))
		time.Sleep(15 * time.Millisecond)
	}
}

// Raw returns everything hmj has written so far, escape sequences included.
func (s *session) Raw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.raw)
}

// Screen returns everything hmj has written so far with escape sequences and carriage
// returns removed.
func (s *session) Screen() string { return stripANSI(s.Raw()) }

// Expect waits until substr appears after the cursor, then moves the cursor past it. Each
// expectation therefore only matches output that came after the previous one, never a
// stale copy of the same text printed earlier (a replayed transcript, an echoed prompt).
func (s *session) Expect(substr string) {
	s.t.Helper()
	s.expect(substr, func(screen string) (int, int) {
		i := strings.Index(screen, substr)
		return i, i + len(substr)
	})
}

// ExpectRegexp is Expect for a regular expression.
func (s *session) ExpectRegexp(pattern string) {
	s.t.Helper()
	re := regexp.MustCompile(pattern)
	s.expect(pattern, func(screen string) (int, int) {
		if loc := re.FindStringIndex(screen); loc != nil {
			return loc[0], loc[1]
		}
		return -1, -1
	})
}

func (s *session) expect(what string, find func(string) (int, int)) {
	s.t.Helper()
	deadline := time.Now().Add(s.timeout)
	for {
		screen := s.Screen()
		s.mu.Lock()
		from := min(s.cursor, len(screen))
		s.mu.Unlock()
		if start, end := find(screen[from:]); start >= 0 {
			s.mu.Lock()
			s.cursor = from + end
			s.mu.Unlock()
			return
		}
		select {
		case <-s.exited:
			// Give the reader a moment to drain what the process wrote before exiting.
			time.Sleep(50 * time.Millisecond)
			if start, _ := find(s.Screen()[from:]); start >= 0 {
				continue
			}
			s.t.Fatalf("hmj exited (status %d) before printing %q; output after cursor:\n%s", s.exitCode, what, tail(s.Screen()[from:], 2000))
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %q; output after cursor:\n%s", what, tail(screen[from:], 2000))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Since returns the stripped output after the cursor without moving it.
func (s *session) Since() string {
	screen := s.Screen()
	s.mu.Lock()
	defer s.mu.Unlock()
	return screen[min(s.cursor, len(screen)):]
}

// ExpectPrompt waits for the input prompt that follows a finished command or turn.
func (s *session) ExpectPrompt() {
	s.t.Helper()
	s.ExpectRegexp(`\[model: [^\]]+\]\n> `)
}

// Quit presses Ctrl+D and requires a clean exit.
func (s *session) Quit() {
	s.t.Helper()
	s.Type("\x04")
	if code := s.Wait(); code != 0 {
		s.t.Fatalf("exit status %d after Ctrl+D, want 0", code)
	}
}

// Wait waits for hmj to exit and returns its exit status.
func (s *session) Wait() int {
	s.t.Helper()
	select {
	case <-s.exited:
		<-s.readDone
		return s.exitCode
	case <-time.After(s.timeout):
		s.t.Fatalf("hmj did not exit; output after cursor:\n%s", tail(s.Since(), 2000))
		return -1
	}
}

// Resize changes the terminal size; hmj receives SIGWINCH. ScreenState applies the change
// from this point of the output on.
func (s *session) Resize(rows, cols int) {
	s.t.Helper()
	s.mu.Lock()
	s.resizes = append(s.resizes, screenResize{offset: len(s.raw), rows: rows, cols: cols})
	s.mu.Unlock()
	if err := unix.IoctlSetWinsize(int(s.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)}); err != nil {
		s.t.Fatal(err)
	}
}

// ScreenState renders everything hmj has written so far on an emulated terminal of the
// session's size: what the user sees right now.
func (s *session) ScreenState() *vtScreen {
	s.mu.Lock()
	raw, resizes := string(s.raw), slices.Clone(s.resizes)
	s.mu.Unlock()
	return renderScreen(raw, s.rows, s.cols, resizes)
}

// ExpectScreen waits until the rendered screen satisfies cond and returns it. On timeout the
// test fails with a picture of the screen, cursor included.
func (s *session) ExpectScreen(what string, cond func(*vtScreen) bool) *vtScreen {
	s.t.Helper()
	deadline := time.Now().Add(s.timeout)
	for {
		v := s.ScreenState()
		if cond(v) {
			return v
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting until the screen shows %s; %s", what, v.Dump())
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Signal sends sig to the hmj process.
func (s *session) Signal(sig syscall.Signal) {
	s.t.Helper()
	if err := s.cmd.Process.Signal(sig); err != nil {
		s.t.Fatal(err)
	}
}

// ansiRe matches CSI, OSC and the short escape sequences hmj writes.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[=>78]`)

func stripANSI(s string) string {
	return strings.ReplaceAll(ansiRe.ReplaceAllString(s, ""), "\r", "")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// openPTY opens a pseudo terminal pair. The slave becomes the child's controlling terminal.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	ptn, err := unix.IoctlGetUint32(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptn), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// waitUntil polls cond until it holds or the timeout passes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(expectTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// conversationFile returns the file of conversation id in the session directory dir.
func conversationFile(dir, id string) string {
	return filepath.Join(dir, "conversations", id+".json")
}
