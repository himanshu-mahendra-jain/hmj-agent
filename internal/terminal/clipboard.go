package terminal

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"hmj-agent/v2/internal/terminal/theme"
)

const maxOSC52Bytes = 100 * 1024

// CopyToClipboard copies text to the system clipboard using OSC 52
// ANSI escape sequences and native OS clipboard utilities. It returns an
// error when no channel actually accepted the text, so callers never report
// a false success. Escape sequences and control characters are removed first:
// pasted into a shell, an ESC [201~ in the model's text would end the bracketed
// paste and run what follows as typed.
func CopyToClipboard(text string) error {
	text = theme.StripControls(text)
	var (
		osc52Sent  bool
		osc52Skips []string
	)

	// 1. Emit OSC 52 ANSI escape sequence if stdout is a terminal and text size is bounded.
	// This prevents polluting piped output and avoids terminal freezes on huge payloads.
	switch {
	case !theme.IsTerminal(os.Stdout.Fd()):
		osc52Skips = append(osc52Skips, "stdout is not a terminal")
	case len(text) > maxOSC52Bytes:
		osc52Skips = append(osc52Skips, fmt.Sprintf("text exceeds the %dKB OSC 52 limit", maxOSC52Bytes/1024))
	default:
		encoded := base64.StdEncoding.EncodeToString([]byte(text))
		osc52 := fmt.Sprintf("\x1b]52;c;%s\x07", encoded)
		if _, err := os.Stdout.WriteString(osc52); err != nil {
			osc52Skips = append(osc52Skips, fmt.Sprintf("OSC 52 write failed: %v", err))
		} else {
			osc52Sent = true
		}
	}

	// 2. Try OS-native clipboard commands when available (Wayland wl-copy or X11 xclip/xsel).
	var cmd *exec.Cmd
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if path, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command(path)
		}
	}
	if cmd == nil {
		if path, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command(path, "-selection", "clipboard")
		} else if path, err := exec.LookPath("xsel"); err == nil {
			cmd = exec.Command(path, "-b", "-i")
		}
	}

	if cmd != nil {
		cmd.Stdin = strings.NewReader(text)
		nativeErr := cmd.Run()
		if nativeErr == nil {
			return nil
		}
		// A failing native utility must not mask a successful OSC 52 copy:
		// over SSH, xclip/xsel are often installed but have no X display to
		// open while the OSC 52 escape already landed on the user's terminal.
		if osc52Sent {
			return nil
		}
		return fmt.Errorf("native clipboard command failed: %w", nativeErr)
	}

	if osc52Sent {
		return nil
	}
	return fmt.Errorf("clipboard unavailable: %s and no native clipboard utility (wl-copy, xclip, xsel) found",
		strings.Join(osc52Skips, "; "))
}
