package theme

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// accessibilityMode is set by SetAccessibility. It is atomic because spinner goroutines read it.
var accessibilityMode atomic.Bool

// accessibilityEnv is the ACCESSIBILITY environment variable, read once: Accessibility is called on
// every prompt render and spinner frame, and the environment does not change during a run.
var accessibilityEnv = sync.OnceValue(func() bool {
	v := os.Getenv("ACCESSIBILITY")
	return v == "1" || strings.EqualFold(v, "true")
})

// SetAccessibility forces accessibility mode on or off. Exposed for tests and config.
func SetAccessibility(on bool) { accessibilityMode.Store(on) }

// Accessibility reports whether high-contrast / screen-reader accessibility mode is active.
// SetAccessibility takes precedence over the ACCESSIBILITY environment variable.
func Accessibility() bool {
	if accessibilityMode.Load() {
		return true
	}
	return accessibilityEnv()
}

// asciiFallback maps the non-ASCII glyphs used in interface chrome onto plain text.
var asciiFallback = strings.NewReplacer(
	// Listed before "·" so a hunk separator reads as an ellipsis, not "|||".
	"···", "...",
	"↑", "Up", "↓", "Down", "—", "-", "−", "-", "…", "...", "·", "|",
	"•", "*", "★", "*", "❯", ">", "▾", ">", "▸", ">", "→", "->", "│", "|",
)

// ASCII returns s unchanged, or in accessibility mode with the interface glyphs replaced
// by ASCII text (arrows, dashes, ellipses, bullets). Use it on chrome text only; model and
// file content is passed through as-is.
func ASCII(s string) string {
	if !Accessibility() {
		return s
	}
	return asciiFallback.Replace(s)
}

// --- Status lines -----------------------------------------------------------

func OK(s string) string {
	if Accessibility() {
		return Style("[OK] "+s, pal.GlyphAdd...)
	}
	return Style("✔ "+s, pal.GlyphAdd...)
}

func Note(s string) string {
	if Accessibility() {
		return Style("[NOTE] "+s, pal.Info...)
	}
	return Style("ℹ "+s, pal.Info...)
}

func Danger(s string) string {
	if Accessibility() {
		return Style("[ERROR] "+s, pal.Error...)
	}
	return Style("✗ "+s, pal.Error...)
}

func Caution(s string) string {
	if Accessibility() {
		return Style("[WARN] "+s, pal.Warn...)
	}
	return Style("⚠ "+s, pal.Warn...)
}

func Hint(s string) string {
	if Accessibility() {
		return Style("[HINT] "+s, pal.Muted...)
	}
	return Style("→ "+s, pal.Muted...)
}

// ToolBadge returns a vibrant, semantic icon and colored badge for each tool call.
func ToolBadge(name string) string {
	if !enabled {
		if Accessibility() {
			return "[TOOL " + name + "]:"
		}
		return name + ":"
	}
	if Accessibility() {
		return Style("[TOOL "+name+"]:", pal.Tool...)
	}
	switch name {
	case "read_file":
		return Style("📖 read_file:", pal.Tool...)
	case "write_file":
		return Style("📝 write_file:", pal.Tool...)
	case "edit_file":
		return Style("✏️  edit_file:", pal.Tool...)
	case "list_files":
		return Style("📁 list_files:", pal.Tool...)
	case "grep":
		return Style("🔎 grep:", pal.Tool...)
	case "run_command":
		return Style("⚡ run_command:", pal.Tool...)
	case "web_search":
		return Style("🔍 web_search:", pal.Tool...)
	case "fetch_url":
		return Style("🌐 fetch_url:", pal.Tool...)
	default:
		return Style("⚙️ "+name+":", pal.Tool...)
	}
}
