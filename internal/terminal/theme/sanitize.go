package theme

import "strings"

// Sanitize neutralises control characters in text we do not control (file
// contents, command output, replayed messages): ANSI/VT sequences are dropped,
// carriage returns are removed so output cannot rewrite its own line, and the
// remaining C0/C1 controls are stripped. Tabs and newlines survive.
func Sanitize(s string) string {
	s = StripANSI(s)
	s = stripC1Runes(s)
	if !strings.ContainsRune(s, '\r') && !hasControl(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r == '\r':
			// drop: line rewriting is never intended in captured output
		case r < 0x20, r >= 0x7f && r <= 0x9f:
			// drop other controls
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hasControl(s string) bool {
	for i := range len(s) {
		c := s[i]
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			return true
		}
		if c == 0x7f {
			return true
		}
	}
	return false
}

// stripC1Runes removes C1 control runes (U+0080–U+009F) from s.
func stripC1Runes(s string) string {
	if !strings.ContainsFunc(s, isC1) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !isC1(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isC1(r rune) bool { return r >= 0x80 && r <= 0x9f }

// StripANSI removes ANSI/VT100 escape sequences from s. Text copied out of
// terminal output carries colour codes that would corrupt cursor tracking and
// visible-width maths, so it must be scrubbed before being echoed or measured.
func StripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) {
			i++ // consume ESC
			switch {
			case s[i] == '[':
				// CSI: ESC [ <params> <final byte 0x40–0x7E>
				i++
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
				if i < len(s) {
					i++ // consume final byte
				}
			case s[i] == ']':
				// OSC: ESC ] ... ST (BEL 0x07 or ESC \)
				i++
				for i < len(s) {
					if s[i] == 0x07 {
						i++
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			default:
				// Two-char sequence: ESC <char>
				i++
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
