package e2e

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sgrRe matches SGR (color and style) sequences and captures their parameters.
var sgrRe = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// sgrUse summarizes the SGR styles found in some output.
type sgrUse struct {
	dim        bool // parameter 2
	extended   bool // 256-color or true-color foreground or background (38;5 / 38;2 / 48;…)
	background bool // any background color (40–47, 100–107 or 48;…)
	bright     bool // a bright foreground (90–97)
}

// scanSGR reads every SGR sequence in out. Extended color forms are skipped as a unit so
// their numbers are not mistaken for styles.
func scanSGR(out string) sgrUse {
	var u sgrUse
	for _, m := range sgrRe.FindAllStringSubmatch(out, -1) {
		params := strings.Split(m[1], ";")
		for i := 0; i < len(params); i++ {
			n, _ := strconv.Atoi(params[i])
			switch {
			case (n == 38 || n == 48) && i+1 < len(params):
				u.extended = true
				u.background = u.background || n == 48
				if params[i+1] == "5" {
					i += 2
				} else {
					i += 4
				}
			case n == 2:
				u.dim = true
			case (n >= 40 && n <= 47) || (n >= 100 && n <= 107):
				u.background = true
			case n >= 90 && n <= 97:
				u.bright = true
			}
		}
	}
	return u
}

// runStyledSession runs a turn and opens a menu (with muted help text) and returns the raw
// output.
func runStyledSession(t *testing.T, args ...string) string {
	t.Helper()
	p := newProvider(t)
	_, s := start(t, p, args...)
	s.Type("hi\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Type("\x12") // Ctrl+R
	s.Expect("select conversation")
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
	return s.Raw()
}

// TestHighContrastUsesBrightBasicColors checks that --high-contrast draws only with bright
// basic colors: no dim text, no 256-color shades (such as the muted grey-blue hints) and no
// background fills. The default palette is checked to use 256-color shades, so the test
// shows a real difference.
func TestHighContrastUsesBrightBasicColors(t *testing.T) {
	t.Parallel()
	hc := scanSGR(runStyledSession(t, "--high-contrast"))
	if hc.dim || hc.extended || hc.background {
		t.Errorf("--high-contrast output: dim=%v 256/true-color=%v background=%v, want none", hc.dim, hc.extended, hc.background)
	}
	if !hc.bright {
		t.Errorf("--high-contrast output uses no bright colors")
	}
	if def := scanSGR(runStyledSession(t)); !def.extended {
		t.Errorf("the default palette uses no 256-color shades, so the high-contrast check shows no difference")
	}
}
