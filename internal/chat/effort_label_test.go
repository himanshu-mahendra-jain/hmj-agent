package chat

import (
	"testing"

	"hmj-agent/internal/config"
)

// TestEffortLabel checks what the effort label says for each protocol and setting.
func TestEffortLabel(t *testing.T) {
	for _, tc := range []struct {
		api, baseURL, effort, thinking, want string
	}{
		{config.APIAnthropic, "", "high", "", "high"},
		{config.APIAnthropic, "", "", "", "default"},
		{config.APIAnthropic, "", "max", "off", "max"}, // Anthropic sends effort without thinking too
		{config.APIResponses, "", "xhigh", "", "xhigh"},
		{config.APIResponses, "", "low", "off", "default (low not sent: thinking off)"},
		{config.APIGemini, "", "medium", "", "medium"},
		{config.APIGemini, "", "xhigh", "", "high (from xhigh)"},
		{config.APIGemini, "", "max", "", "high (from max)"},
		{config.APIGemini, "", "high", "off", "default (high not sent: thinking off)"},
		{config.APICompletions, "", "high", "", "default (high not sent: Chat Completions)"},
		{config.APICompletions, "", "", "", "default"},
		{"", "https://api.anthropic.com", "high", "", "high"},       // inferred from the URL
		{"", "http://localhost:8080/v1", "medium", "off", "medium"}, // found by probing: as configured
	} {
		p := config.Provider{API: tc.api, BaseURL: tc.baseURL}
		m := config.Model{ID: "m", Effort: tc.effort, Thinking: tc.thinking}
		if got := EffortLabel(p, m); got != tc.want {
			t.Errorf("%s %s effort %q thinking %q: got %q, want %q", tc.api, tc.baseURL, tc.effort, tc.thinking, got, tc.want)
		}
	}
}
