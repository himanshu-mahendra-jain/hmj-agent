package redact

import "testing"

// A registered secret that contains another registered secret is masked whole, not
// left with a fragment of its prefix or suffix showing.
func TestRegisteredSecretContainingAnother(t *testing.T) {
	Register("inner-secret-1234", "outer-inner-secret-1234-tail")
	got := String("a outer-inner-secret-1234-tail b inner-secret-1234 c")
	if want := "a [REDACTED] b [REDACTED] c"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
