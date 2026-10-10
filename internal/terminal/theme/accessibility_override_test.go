package theme

import "testing"

// TestSetAccessibilityOverridesEnvironment checks that forcing accessibility on wins over the cached
// environment value, and that turning it off again leaves the environment decision in place.
func TestSetAccessibilityOverridesEnvironment(t *testing.T) {
	t.Cleanup(func() { SetAccessibility(false) })

	SetAccessibility(true)
	if !Accessibility() {
		t.Fatal("SetAccessibility(true) must enable accessibility")
	}
	SetAccessibility(false)
	if Accessibility() != accessibilityEnv() {
		t.Fatal("SetAccessibility(false) must fall back to the environment value")
	}
}
