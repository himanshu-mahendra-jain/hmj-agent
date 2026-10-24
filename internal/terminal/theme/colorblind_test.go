package theme

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// Colour-vision-deficiency check of the default palette. Each 256-colour code is converted
// to sRGB, passed through the Machado et al. (2009) full-severity simulation matrices for
// protanopia, deuteranopia and tritanopia, and compared in CIELAB (ΔE76) and by WCAG
// contrast ratio.

var cvdMatrices = map[string][3][3]float64{
	"normal":       {{1, 0, 0}, {0, 1, 0}, {0, 0, 1}},
	"protanopia":   {{0.152286, 1.052583, -0.204868}, {0.114503, 0.786281, 0.099216}, {-0.003882, -0.048116, 1.051998}},
	"deuteranopia": {{0.367322, 0.860646, -0.227968}, {0.280085, 0.672501, 0.047413}, {-0.011820, 0.042940, 0.968881}},
	"tritanopia":   {{1.255528, -0.076749, -0.178779}, {-0.078411, 0.930809, 0.147602}, {0.004733, 0.691367, 0.303900}},
}

type rgb [3]float64 // linear-light sRGB, 0..1

// xterm256 returns the linear RGB of a 256-colour palette index (cube and grey ramp).
func xterm256(i int) rgb {
	level := func(v int) float64 {
		if v == 0 {
			return 0
		}
		return float64(55 + 40*v)
	}
	var c [3]float64
	if i >= 232 {
		g := float64(8 + 10*(i-232))
		c = [3]float64{g, g, g}
	} else {
		i -= 16
		c = [3]float64{level(i / 36), level(i / 6 % 6), level(i % 6)}
	}
	for k := range c {
		v := c[k] / 255
		if v <= 0.04045 {
			c[k] = v / 12.92
		} else {
			c[k] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return c
}

func (c rgb) simulate(m [3][3]float64) rgb {
	var o rgb
	for i := range 3 {
		o[i] = math.Max(0, math.Min(1, m[i][0]*c[0]+m[i][1]*c[1]+m[i][2]*c[2]))
	}
	return o
}

func (c rgb) luminance() float64 { return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2] }

func (c rgb) lab() [3]float64 {
	x := (0.4124*c[0] + 0.3576*c[1] + 0.1805*c[2]) / 0.95047
	y := c.luminance()
	z := (0.0193*c[0] + 0.1192*c[1] + 0.9505*c[2]) / 1.08883
	f := func(t float64) float64 {
		if t > 0.008856 {
			return math.Cbrt(t)
		}
		return 7.787*t + 16.0/116
	}
	return [3]float64{116*f(y) - 16, 500 * (f(x) - f(y)), 200 * (f(y) - f(z))}
}

func deltaE(a, b rgb) float64 {
	la, lb := a.lab(), b.lab()
	return math.Sqrt((la[0]-lb[0])*(la[0]-lb[0]) + (la[1]-lb[1])*(la[1]-lb[1]) + (la[2]-lb[2])*(la[2]-lb[2]))
}

func contrastRatio(a, b rgb) float64 {
	x, y := a.luminance()+0.05, b.luminance()+0.05
	return math.Max(x, y) / math.Min(x, y)
}

// colorIndex extracts the 256-colour index from codes such as "38;5;114" or "48;5;52".
func colorIndex(t *testing.T, codes []string, prefix string) int {
	t.Helper()
	for _, c := range codes {
		if rest, ok := strings.CutPrefix(c, prefix); ok {
			n, err := strconv.Atoi(rest)
			if err != nil {
				t.Fatalf("bad colour code %q", c)
			}
			return n
		}
	}
	t.Fatalf("no %s colour in %v", prefix, codes)
	return 0
}

// Every foreground stays readable (WCAG AA, 4.5:1) on a dark terminal background and on
// the added-row background, whatever the viewer's colour vision.
func TestPaletteContrastUnderColorBlindness(t *testing.T) {
	p := DefaultPalette()
	roles := map[string][]string{
		"Text": p.Text, "Muted": p.Muted, "Faint": p.Faint, "Prompt": p.Prompt, "Tool": p.Tool,
		"Added": p.Added, "Removed": p.Removed, "Info": p.Info, "Warn": p.Warn, "Error": p.Error,
		"Thinking": p.ThinkingParam,
	}
	backgrounds := map[string]rgb{"black": {0, 0, 0}, "added row": xterm256(colorIndex(t, p.addedRow.bg, "48;5;"))}
	for vision, m := range cvdMatrices {
		for role, codes := range roles {
			fg := xterm256(colorIndex(t, codes, "38;5;")).simulate(m)
			for bgName, bg := range backgrounds {
				if r := contrastRatio(fg, bg.simulate(m)); r < 4.5 {
					t.Errorf("%s: %s on %s has contrast %.1f:1, want at least 4.5:1", vision, role, bgName, r)
				}
			}
		}
	}
}

// Added and removed lines may look alike in hue to some viewers (green and red text differ
// by only ΔE≈6 under deuteranopia), so the difference must not rest on the text colour:
// the row backgrounds stay clearly distinct and the diff markers are bold.
func TestDiffRowsDistinguishableUnderColorBlindness(t *testing.T) {
	p := DefaultPalette()
	addedBG := xterm256(colorIndex(t, p.addedRow.bg, "48;5;"))
	removedBG := xterm256(colorIndex(t, p.removedRow.bg, "48;5;"))
	for vision, m := range cvdMatrices {
		if d := deltaE(addedBG.simulate(m), removedBG.simulate(m)); d < 10 {
			t.Errorf("%s: added and removed row backgrounds differ by ΔE %.1f, want at least 10", vision, d)
		}
	}
	for _, pal := range []Palette{DefaultPalette(), HighContrastPalette()} {
		if !strings.Contains(strings.Join(pal.MarkAdd, ";"), paramBold) || !strings.Contains(strings.Join(pal.MarkDel, ";"), paramBold) {
			t.Error("diff markers are not bold")
		}
	}
	if !strings.Contains(strings.Join(HighContrastPalette().Removed, ";"), paramUnderline) {
		t.Error("high-contrast removed text is not underlined")
	}
}

// Status lines never rely on colour: each kind has its own symbol, and its own text badge
// in accessible mode.
func TestStatusLinesHaveNonColorCues(t *testing.T) {
	defer SetAccessibility(Accessibility())
	for _, accessible := range []bool{false, true} {
		SetAccessibility(accessible)
		seen := map[string]bool{}
		for _, line := range []string{OK("x"), Note("x"), Danger("x"), Caution("x"), Hint("x")} {
			prefix := StripANSI(line)
			if seen[prefix] {
				t.Errorf("accessible=%v: two status kinds share the prefix %q", accessible, prefix)
			}
			seen[prefix] = true
		}
	}
}
