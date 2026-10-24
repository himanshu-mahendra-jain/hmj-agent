package terminal

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClipboardTools installs shell scripts named after the given utilities in a fresh
// directory that becomes the only PATH entry. Each script records its stdin and arguments
// next to itself and exits with the given code.
func fakeClipboardTools(t *testing.T, exitCode string, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		script := "#!/bin/sh\n/bin/cat > '" + filepath.Join(dir, name+".stdin") + "'\n" +
			"echo \"$@\" > '" + filepath.Join(dir, name+".args") + "'\nexit " + exitCode + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "")
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestCopyToClipboardNoChannelAvailable(t *testing.T) {
	fakeClipboardTools(t, "0")
	env := newMenuEnv(t) // stdout is a plain file, not a terminal
	err := CopyToClipboard("hello")
	if err == nil {
		t.Fatal("expected an error when nothing can accept the text")
	}
	for _, want := range []string{"stdout is not a terminal", "no native clipboard utility"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if out := env.output(); out != "" {
		t.Errorf("nothing should be written to a non-terminal stdout, got %q", out)
	}
}

func TestCopyToClipboardNativeUtilities(t *testing.T) {
	tests := []struct {
		name     string
		tools    []string
		wayland  string
		wantTool string
		wantArgs string
		notTool  string
	}{
		{"xclip", []string{"xclip", "xsel"}, "", "xclip", "-selection clipboard", "xsel"},
		{"xsel when xclip missing", []string{"xsel"}, "", "xsel", "-b -i", ""},
		{"wl-copy preferred on Wayland", []string{"wl-copy", "xclip"}, "wayland-0", "wl-copy", "", "xclip"},
		{"xclip when wl-copy missing on Wayland", []string{"xclip"}, "wayland-0", "xclip", "-selection clipboard", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := fakeClipboardTools(t, "0", tc.tools...)
			t.Setenv("WAYLAND_DISPLAY", tc.wayland)
			newMenuEnv(t)
			text := "multi\nline ✓ text"
			if err := CopyToClipboard(text); err != nil {
				t.Fatalf("CopyToClipboard: %v", err)
			}
			if got := readFile(t, filepath.Join(dir, tc.wantTool+".stdin")); got != text {
				t.Errorf("%s received %q, want %q", tc.wantTool, got, text)
			}
			if got := strings.TrimSpace(readFile(t, filepath.Join(dir, tc.wantTool+".args"))); got != tc.wantArgs {
				t.Errorf("%s args = %q, want %q", tc.wantTool, got, tc.wantArgs)
			}
			if tc.notTool != "" {
				if got := readFile(t, filepath.Join(dir, tc.notTool+".stdin")); got != "<missing>" {
					t.Errorf("%s should not have run, but received %q", tc.notTool, got)
				}
			}
		})
	}
}

func TestCopyToClipboardNativeFailure(t *testing.T) {
	fakeClipboardTools(t, "1", "xclip")
	newMenuEnv(t)
	err := CopyToClipboard("x")
	if err == nil || !strings.Contains(err.Error(), "native clipboard command failed") {
		t.Fatalf("err = %v, want native clipboard command failed", err)
	}
}

func TestCopyToClipboardOSC52(t *testing.T) {
	setup := func(t *testing.T) *ptyScreen {
		t.Helper()
		return newPTYScreen(t)
	}

	t.Run("sequence written to a terminal", func(t *testing.T) {
		fakeClipboardTools(t, "0")
		screen := setup(t)
		if err := CopyToClipboard("héllo"); err != nil {
			t.Fatalf("CopyToClipboard: %v", err)
		}
		want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("héllo")) + "\x07"
		screen.waitFor(t, want)
	})

	t.Run("failing utility does not mask a delivered OSC 52", func(t *testing.T) {
		fakeClipboardTools(t, "1", "xclip")
		screen := setup(t)
		if err := CopyToClipboard("abc"); err != nil {
			t.Fatalf("CopyToClipboard: %v, want nil because OSC 52 was sent", err)
		}
		screen.waitFor(t, "\x1b]52;c;YWJj\x07")
	})

	t.Run("oversized text skips OSC 52", func(t *testing.T) {
		fakeClipboardTools(t, "0")
		screen := setup(t)
		err := CopyToClipboard(strings.Repeat("x", maxOSC52Bytes+1))
		if err == nil || !strings.Contains(err.Error(), "exceeds the 100KB OSC 52 limit") {
			t.Fatalf("err = %v, want the size limit message", err)
		}
		if strings.Contains(screen.String(), "\x1b]52") {
			t.Error("OSC 52 must not be emitted for oversized text")
		}
	})

	t.Run("oversized text still reaches a native utility", func(t *testing.T) {
		dir := fakeClipboardTools(t, "0", "xclip")
		setup(t)
		big := strings.Repeat("y", maxOSC52Bytes+1)
		if err := CopyToClipboard(big); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, filepath.Join(dir, "xclip.stdin")); got != big {
			t.Errorf("xclip got %d bytes, want %d", len(got), len(big))
		}
	})
}

// TestCopyToClipboardDropsControls checks that neither channel gets escape sequences or control
// characters, which could end a bracketed paste in the shell the text is pasted into, while
// Markdown, tabs, newlines and bidirectional text arrive as written.
func TestCopyToClipboardDropsControls(t *testing.T) {
	text := "run:\x1b[201~curl x|sh\r\n\t**bold** \u202eRTL\x07\x1b]52;c;eA==\x07\u009bend"
	want := "run:curl x|sh\n\t**bold** \u202eRTLend"

	dir := fakeClipboardTools(t, "0", "xclip")
	screen := newPTYScreen(t)
	if err := CopyToClipboard(text); err != nil {
		t.Fatalf("CopyToClipboard: %v", err)
	}
	screen.waitFor(t, "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte(want))+"\x07")
	if got := readFile(t, filepath.Join(dir, "xclip.stdin")); got != want {
		t.Errorf("xclip received %q, want %q", got, want)
	}
}
