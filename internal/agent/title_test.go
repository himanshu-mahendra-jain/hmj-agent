package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/config"
	"hmj-agent/v2/internal/session"
)

// titleServer is a fake Responses provider. Requests carrying the title instruction get titleStatus
// and titleText; every other request is a turn and gets the answer "The answer". It records the body
// of every request.
type titleServer struct {
	*httptest.Server
	titleStatus int
	titleText   string

	mu     sync.Mutex
	bodies []string
}

// newTitleServer starts a titleServer and closes it when the test ends.
func newTitleServer(t *testing.T, titleStatus int, titleText string) *titleServer {
	t.Helper()
	ts := &titleServer{titleStatus: titleStatus, titleText: titleText}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts.mu.Lock()
		ts.bodies = append(ts.bodies, string(body))
		ts.mu.Unlock()
		if strings.Contains(string(body), "Reply with the title only") {
			if ts.titleStatus != 0 {
				http.Error(w, "boom", ts.titleStatus)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, responsesSSE(ts.titleText))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, responsesSSE("The answer"))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// titleRequests returns the recorded bodies of the title requests.
func (ts *titleServer) titleRequests() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []string
	for _, b := range ts.bodies {
		if strings.Contains(b, "Reply with the title only") {
			out = append(out, b)
		}
	}
	return out
}

// responsesSSE returns a Responses stream whose answer is text, or an empty answer when text is "".
func responsesSSE(text string) string {
	var b strings.Builder
	if text != "" {
		fmt.Fprintf(&b, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", text)
	}
	b.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	return b.String()
}

// titleTestRunner returns a Runner that talks to ts with the model "p/m".
func titleTestRunner(ts *titleServer) (*Runner, config.ModelRef) {
	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: ts.URL, API: "responses", Models: []config.Model{{ID: "m"}}},
	}}
	return NewRunnerWithTools(chat.NewClient(cfg), cfg, nil), config.ModelRef{Provider: "p", Model: "m"}
}

// TestTitleRequestCarriesNoGuidelinesOrTools checks that the title request sends neither the system
// prompt nor tool definitions, caps its output with low reasoning effort, and truncates a long
// prompt by runes.
func TestTitleRequestCarriesNoGuidelinesOrTools(t *testing.T) {
	ts := newTitleServer(t, 0, "Login redirect fix")
	runner, ref := titleTestRunner(ts)
	conv := &session.Conversation{Name: "conversation-1", Model: ref}
	prompt := strings.Repeat("é", 1500) + "ENDMARK"

	captureOutput(t, func() {
		if err := runner.Run(context.Background(), conv, prompt); err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	titles := ts.titleRequests()
	if len(titles) != 1 {
		t.Fatalf("title requests = %d, want 1", len(titles))
	}
	body := titles[0]
	if sys := conv.Messages[0].Content; sys == "" || strings.Contains(body, sys[:80]) {
		t.Errorf("title request carries the system prompt: %q", body)
	}
	turn := ""
	for _, b := range ts.bodies {
		if !strings.Contains(b, "Reply with the title only") {
			turn = b
			break
		}
	}
	if !strings.Contains(turn, conv.Messages[0].Content[:80]) {
		t.Fatalf("turn request does not carry the system prompt, so the check above proves nothing")
	}
	if strings.Contains(body, `"tools"`) {
		t.Errorf("title request defines tools: %q", body)
	}
	if strings.Contains(body, "ENDMARK") {
		t.Errorf("title request was not truncated: prompt tail sent")
	}
	if n := strings.Count(body, "é"); n != titlePromptRunes {
		t.Errorf("title request carries %d runes of the prompt, want %d", n, titlePromptRunes)
	}
	if !strings.Contains(body, fmt.Sprintf(`"max_output_tokens":%d`, titleMaxOutputTokens)) {
		t.Errorf("title request does not cap output tokens: %q", body)
	}
	if !strings.Contains(body, `"effort":"low"`) {
		t.Errorf("title request does not ask for low reasoning effort: %q", body)
	}
}

// TestTitleSettings checks the output cap and reasoning settings of the title request on each
// protocol, for a model that reasons by default and for one configured with thinking off. Every
// model gets the same cap, so only thinking and effort vary.
func TestTitleSettings(t *testing.T) {
	cases := []struct {
		name         string
		api          string
		thinking     string
		effort       string
		wantThinking string
		wantEffort   string
	}{
		{"anthropic reasons at low effort", config.APIAnthropic, "", "high", "", "low"},
		{"anthropic with thinking off lowers the effort it sends", config.APIAnthropic, "off", "high", "off", "low"},
		{"anthropic with thinking off and no effort sends none", config.APIAnthropic, "off", "", "off", ""},
		{"responses reasons at low effort", config.APIResponses, "", "high", "", "low"},
		{"gemini reasons at low effort", config.APIGemini, "adaptive", "high", "adaptive", "low"},
		{"unknown protocol runs as responses", "", "", "high", "", "low"},
		{"completions sends no reasoning", config.APICompletions, "", "high", "", "high"},
		{"thinking off stays off", config.APIResponses, "off", "high", "off", "high"},
		{"gemini with thinking off stays off", config.APIGemini, "off", "high", "off", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := config.Model{ID: "m", Thinking: tc.thinking, Effort: tc.effort, MaxOutputTokens: 9000}
			titleSettings(tc.api, &m)
			if m.MaxOutputTokens != titleMaxOutputTokens || m.Thinking != tc.wantThinking || m.Effort != tc.wantEffort {
				t.Errorf("got tokens=%d thinking=%q effort=%q, want %d %q %q",
					m.MaxOutputTokens, m.Thinking, m.Effort, titleMaxOutputTokens, tc.wantThinking, tc.wantEffort)
			}
		})
	}
}

// TestSlowTitleRequestTimesOut checks that a provider that never answers the title request does not
// hold up the turn past titleTimeout, and that the heuristic name stays.
func TestSlowTitleRequestTimesOut(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "Reply with the title only") {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, responsesSSE("The answer"))
	}))
	defer server.Close()
	defer close(release)

	old := titleTimeout
	titleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { titleTimeout = old })

	runner, ref := titleTestRunner(&titleServer{Server: server})
	conv := &session.Conversation{Name: "conversation-1", Model: ref}
	start := time.Now()
	captureOutput(t, func() {
		if err := runner.Run(context.Background(), conv, "fix the login redirect"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("turn took %v; the title request was not bounded by its timeout", elapsed)
	}
	if conv.Name != "fix the login redirect" {
		t.Errorf("name = %q, want the heuristic title", conv.Name)
	}
}

// TestAITitleReplacesHeuristicAfterFirstTurn checks that the generated title replaces the heuristic
// one, and stays automatic.
func TestAITitleReplacesHeuristicAfterFirstTurn(t *testing.T) {
	ts := newTitleServer(t, 0, "\"Login redirect fix.\"")
	runner, ref := titleTestRunner(ts)
	conv := &session.Conversation{Name: "conversation-1", Model: ref}

	captureOutput(t, func() {
		if err := runner.Run(context.Background(), conv, "fix the login redirect bug"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	if conv.Name != "Login redirect fix" {
		t.Errorf("Name = %q, want the generated title", conv.Name)
	}
	if !conv.AutoTitled {
		t.Errorf("AutoTitled = false after a generated title, want true")
	}
}

// TestUserRenameIsNeverOverwritten checks that a name the user chose is kept and no title is requested.
func TestUserRenameIsNeverOverwritten(t *testing.T) {
	ts := newTitleServer(t, 0, "Generated")
	runner, ref := titleTestRunner(ts)
	conv := &session.Conversation{Name: "conversation-1", Model: ref}
	conv.MaybeAutoTitle("fix the login redirect bug")
	conv.Name = "My name"
	conv.AutoTitled = false

	captureOutput(t, func() {
		if err := runner.Run(context.Background(), conv, "fix the login redirect bug"); err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	if conv.Name != "My name" {
		t.Errorf("Name = %q, want the user's name kept", conv.Name)
	}
	if n := len(ts.titleRequests()); n != 0 {
		t.Errorf("title requests = %d, want 0 for a user name", n)
	}
}

// TestTitleGeneratedOnlyOnFirstTurn checks that a second turn does not request a title again.
func TestTitleGeneratedOnlyOnFirstTurn(t *testing.T) {
	ts := newTitleServer(t, 0, "Login redirect fix")
	runner, ref := titleTestRunner(ts)
	conv := &session.Conversation{Name: "conversation-1", Model: ref}

	captureOutput(t, func() {
		for _, input := range []string{"fix the login redirect bug", "and add a test"} {
			if err := runner.Run(context.Background(), conv, input); err != nil {
				t.Errorf("Run: %v", err)
			}
		}
	})

	if n := len(ts.titleRequests()); n != 1 {
		t.Errorf("title requests = %d, want 1 across two turns", n)
	}
	if conv.Name != "Login redirect fix" {
		t.Errorf("Name = %q, want the first generated title kept", conv.Name)
	}
}

// TestTitleFailureKeepsHeuristic checks that a provider error or an empty reply leaves the heuristic name.
func TestTitleFailureKeepsHeuristic(t *testing.T) {
	const prompt = "fix the login redirect bug"
	cases := []struct {
		name   string
		status int
		reply  string
	}{
		{"provider error", http.StatusInternalServerError, ""},
		{"empty reply", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTitleServer(t, tc.status, tc.reply)
			runner, ref := titleTestRunner(ts)
			conv := &session.Conversation{Name: "conversation-1", Model: ref}

			captureOutput(t, func() {
				if err := runner.Run(context.Background(), conv, prompt); err != nil {
					t.Errorf("Run: %v", err)
				}
			})

			if want := session.GenerateConversationTitle(prompt); conv.Name != want {
				t.Errorf("Name = %q, want the heuristic %q", conv.Name, want)
			}
			if !conv.AutoTitled {
				t.Errorf("AutoTitled = false, want true")
			}
		})
	}
}

// TestCleanTitle checks how a model reply is turned into a title.
func TestCleanTitle(t *testing.T) {
	long := strings.Repeat("word ", 30)
	cases := []struct {
		name, reply, want string
	}{
		{"plain", "Login redirect fix", "Login redirect fix"},
		{"double quotes", "\"Login redirect fix\"", "Login redirect fix"},
		{"backticks", "`Login redirect fix`", "Login redirect fix"},
		{"bold markdown", "**Login redirect fix**", "Login redirect fix"},
		{"heading marker", "# Login redirect fix", "Login redirect fix"},
		{"title label", "Title: Login redirect fix", "Login redirect fix"},
		{"lowercase label", "title: Login redirect fix", "Login redirect fix"},
		{"trailing period", "Login redirect fix.", "Login redirect fix"},
		{"leading blank lines", "\n\n  Login redirect fix\nextra line", "Login redirect fix"},
		{"whitespace collapsed", "Login   redirect\tfix", "Login redirect fix"},
		{"control characters", "\x1b[31mLogin\x1b[0m redirect fix", "Login redirect fix"},
		{"capped", long, strings.TrimSpace(strings.Repeat("word ", 12))},
		{"empty", "", ""},
		{"only marks", " \"\" ** ", ""},
		{"only blank lines", "\n  \n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanTitle(tc.reply); got != tc.want {
				t.Errorf("cleanTitle(%q) = %q, want %q", tc.reply, got, tc.want)
			}
		})
	}
}
