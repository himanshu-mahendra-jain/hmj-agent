package agent

import (
	"slices"
	"strings"
	"testing"
	"time"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
)

func TestCompareHealthResultsOrder(t *testing.T) {
	r := func(prov, model string, status chat.HealthStatus, ms int) chat.ModelHealthResult {
		return chat.ModelHealthResult{
			Ref:     config.ModelRef{Provider: prov, Model: model},
			Status:  status,
			Latency: time.Duration(ms) * time.Millisecond,
		}
	}
	results := []chat.ModelHealthResult{
		r("z", "m", chat.StatusFailed, 5),
		r("b", "slow", chat.StatusHealthy, 300),
		r("a", "tieB", chat.StatusHealthy, 100),
		r("a", "tieA", chat.StatusHealthy, 100),
		r("m", "x", chat.StatusAuthError, 0),
		r("b", "fast", chat.StatusHealthy, 10),
		r("a", "y", chat.StatusTimeout, 9000),
		r("a", "x", chat.StatusRateLimited, 1),
	}
	slices.SortFunc(results, compareHealthResults)
	var got []string
	for _, x := range results {
		got = append(got, x.Ref.Provider+"/"+x.Ref.Model)
	}
	want := []string{"b/fast", "a/tieA", "a/tieB", "b/slow", "a/x", "a/y", "m/x", "z/m"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if compareHealthResults(results[0], results[0]) != 0 {
		t.Error("a result must compare equal to itself")
	}
	if compareHealthResults(r("a", "m", chat.StatusHealthy, 1), r("a", "m", chat.StatusFailed, 1)) != -1 ||
		compareHealthResults(r("a", "m", chat.StatusFailed, 1), r("a", "m", chat.StatusHealthy, 1)) != 1 {
		t.Error("healthy must sort before non-healthy in both argument orders")
	}
}

func TestHealthLabels(t *testing.T) {
	defer theme.SetAccessibility(false)
	statuses := []struct {
		status          chat.HealthStatus
		plain, accessib string
	}{
		{chat.StatusHealthy, "✔ OK", "[OK] OK"},
		{chat.StatusAuthError, "✗ AUTH", "[ERROR] AUTH"},
		{chat.StatusRateLimited, "⚠ RATE", "[WARN] RATE"},
		{chat.StatusTimeout, "⚠ TIMEOUT", "[WARN] TIMEOUT"},
		{chat.StatusFailed, "✗ FAIL", "[ERROR] FAIL"},
		{chat.HealthStatus("???"), "✗ FAIL", "[ERROR] FAIL"},
	}
	for _, accessible := range []bool{false, true} {
		theme.SetAccessibility(accessible)
		for _, tc := range statuses {
			want := tc.plain
			if accessible {
				want = tc.accessib
			}
			if got := healthStatusLabel(tc.status); got != want {
				t.Errorf("accessible=%v healthStatusLabel(%q) = %q, want %q", accessible, tc.status, got, want)
			}
		}
		badge := map[string][2]string{
			"all":  {"✔ health:", "[OK] health:"},
			"some": {"⚠ health:", "[WARN] health:"},
			"none": {"✗ health:", "[ERROR] health:"},
		}
		idx := 0
		if accessible {
			idx = 1
		}
		for name, args := range map[string][2]int{"all": {3, 3}, "some": {2, 3}, "none": {0, 3}} {
			if got := healthSummaryBadge(args[0], args[1]); got != badge[name][idx] {
				t.Errorf("accessible=%v badge(%d,%d) = %q, want %q", accessible, args[0], args[1], got, badge[name][idx])
			}
		}
		if got := healthSummaryBadge(0, 0); got != badge["all"][idx] {
			t.Errorf("no models at all counts as healthy, got %q", got)
		}
	}
}

func TestHealthDetailsAndLatencyLabels(t *testing.T) {
	long := strings.Repeat("e", 61)
	exact := strings.Repeat("e", 60)
	cjk := strings.Repeat("日", 70)
	tests := []struct {
		name string
		r    chat.ModelHealthResult
		want string
	}{
		{"healthy ignores details", chat.ModelHealthResult{Status: chat.StatusHealthy, Details: "whatever"}, "healthy"},
		{"short details kept", chat.ModelHealthResult{Status: chat.StatusFailed, Details: "HTTP 500"}, "HTTP 500"},
		{"exactly 60 runes kept", chat.ModelHealthResult{Status: chat.StatusFailed, Details: exact}, exact},
		{"61 runes truncated", chat.ModelHealthResult{Status: chat.StatusFailed, Details: long}, strings.Repeat("e", 57) + "..."},
		{"truncates by rune not byte", chat.ModelHealthResult{Status: chat.StatusAuthError, Details: cjk}, strings.Repeat("日", 57) + "..."},
		{"empty details", chat.ModelHealthResult{Status: chat.StatusTimeout}, ""},
		{"escapes and line breaks dropped", chat.ModelHealthResult{Status: chat.StatusFailed, Details: "HTTP 500:\x1b]52;c;eA==\x07 bad\n\x1b[2Jgateway"}, "HTTP 500: bad gateway"},
	}
	for _, tc := range tests {
		if got := healthDetailsLabel(tc.r); got != tc.want {
			t.Errorf("%s: healthDetailsLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := healthLatencyLabel(chat.ModelHealthResult{Status: chat.StatusHealthy, Latency: 1234 * time.Millisecond}); got != "1234ms" {
		t.Errorf("latency label = %q", got)
	}
	if got := healthLatencyLabel(chat.ModelHealthResult{Status: chat.StatusTimeout, Latency: time.Second}); got != "—" {
		t.Errorf("timeout latency = %q, want a dash", got)
	}
	if got := healthLatencyLabel(chat.ModelHealthResult{Status: chat.StatusFailed}); got != "—" {
		t.Errorf("zero latency = %q, want a dash", got)
	}
}

func newResumeManager(t *testing.T, names ...string) (*session.SessionManager, string) {
	t.Helper()
	path := t.TempDir()
	model := config.ModelRef{Provider: "p", Model: "m"}
	sm := session.LoadSessionManager(path, model)
	sm.Rename(sm.ActiveID, names[0])
	for _, n := range names[1:] {
		sm.New(n)
	}
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	return sm, path
}

func TestResumeCallbacksRename(t *testing.T) {
	sm, path := newResumeManager(t, "first", "second")
	var msgs []string
	cb := resumeCallbacks(sm, &msgs)
	target := terminal.SelectItem{ID: "2"}

	label, err := cb.OnRename(target, "  renamed  ")
	if err != nil {
		t.Fatal(err)
	}
	if sm.Conversations["2"].Name != "renamed" {
		t.Errorf("name = %q, want trimmed 'renamed'", sm.Conversations["2"].Name)
	}
	if !strings.Contains(label, "[2] renamed (0 user messages, model: p/m)") {
		t.Errorf("label = %q", label)
	}
	if !slices.Equal(msgs, []string{"renamed conversation to:   renamed  "}) {
		t.Errorf("msgs = %q", msgs)
	}
	if reloaded := session.LoadSessionManager(path, config.ModelRef{}); reloaded.Conversations["2"].Name != "renamed" {
		t.Error("rename was not saved to disk")
	}

	msgs = nil
	if _, err := cb.OnRename(target, "   "); err == nil {
		t.Error("blank name must be rejected")
	}
	if _, err := cb.OnRename(terminal.SelectItem{ID: "nope"}, "x"); err == nil {
		t.Error("unknown conversation must be rejected")
	}
	if len(msgs) != 0 || sm.Conversations["2"].Name != "renamed" {
		t.Errorf("failed renames changed state: msgs=%v name=%q", msgs, sm.Conversations["2"].Name)
	}
}

func TestResumeCallbacksDelete(t *testing.T) {
	t.Run("inactive conversation", func(t *testing.T) {
		sm, path := newResumeManager(t, "first", "second", "third")
		active := sm.ActiveID
		var msgs []string
		cb := resumeCallbacks(sm, &msgs)
		if err := cb.OnDelete(terminal.SelectItem{ID: "2"}); err != nil {
			t.Fatal(err)
		}
		if _, ok := sm.Conversations["2"]; ok || len(sm.Conversations) != 2 || sm.ActiveID != active {
			t.Errorf("conversations=%v active=%s (was %s)", sm.Conversations, sm.ActiveID, active)
		}
		if !slices.Equal(msgs, []string{"deleted conversation: second"}) {
			t.Errorf("msgs = %q", msgs)
		}
		if reloaded := session.LoadSessionManager(path, config.ModelRef{}); len(reloaded.Conversations) != 2 {
			t.Errorf("deletion not saved: %d conversations on disk", len(reloaded.Conversations))
		}
	})
	t.Run("active conversation moves the active pointer", func(t *testing.T) {
		sm, _ := newResumeManager(t, "first", "second")
		if sm.ActiveID != "2" {
			t.Fatalf("expected the newest conversation to be active, got %s", sm.ActiveID)
		}
		var msgs []string
		if err := resumeCallbacks(sm, &msgs).OnDelete(terminal.SelectItem{ID: "2"}); err != nil {
			t.Fatal(err)
		}
		if sm.ActiveID != "1" || sm.Active().Name != "first" {
			t.Errorf("active = %s %q, want the remaining conversation", sm.ActiveID, sm.Active().Name)
		}
	})
	t.Run("deleting the only conversation creates a fresh one", func(t *testing.T) {
		sm, _ := newResumeManager(t, "only")
		var msgs []string
		if err := resumeCallbacks(sm, &msgs).OnDelete(terminal.SelectItem{ID: sm.ActiveID}); err != nil {
			t.Fatal(err)
		}
		if len(sm.Conversations) != 1 || sm.Active() == nil || sm.Active().Name != "default" {
			t.Errorf("conversations=%v active=%q", sm.Conversations, sm.ActiveID)
		}
		if !slices.Equal(msgs, []string{"deleted conversation: only"}) {
			t.Errorf("msgs = %q", msgs)
		}
	})
	t.Run("unknown conversation", func(t *testing.T) {
		sm, _ := newResumeManager(t, "first")
		var msgs []string
		if err := resumeCallbacks(sm, &msgs).OnDelete(terminal.SelectItem{ID: "zzz"}); err == nil {
			t.Error("expected an error for an unknown conversation")
		}
		if len(msgs) != 0 || len(sm.Conversations) != 1 {
			t.Errorf("state changed: msgs=%v conversations=%d", msgs, len(sm.Conversations))
		}
	})
}
