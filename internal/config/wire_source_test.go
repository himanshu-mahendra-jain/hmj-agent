package config

import "testing"

func TestWireAPISource(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		api      string
		source   string
	}{
		{"explicit responses", Provider{BaseURL: "https://x/v1", API: "responses"}, APIResponses, SourceExplicit},
		{"explicit wins over url", Provider{BaseURL: "https://api.anthropic.com", API: "completions"}, APICompletions, SourceExplicit},
		{"explicit is case and space insensitive", Provider{BaseURL: "https://x", API: " Anthropic "}, APIAnthropic, SourceExplicit},
		{"gemini host url rule", Provider{BaseURL: "https://generativelanguage.googleapis.com/v1beta"}, APIGemini, SourceURL},
		{"gemini openai path url rule", Provider{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"}, APICompletions, SourceURL},
		{"anthropic host url rule", Provider{BaseURL: "https://api.anthropic.com"}, APIAnthropic, SourceURL},
		{"messages suffix url rule", Provider{BaseURL: "https://gw.example/v1/messages/"}, APIAnthropic, SourceURL},
		{"chat completions suffix url rule", Provider{BaseURL: "https://gw.example/v1/chat/completions"}, APICompletions, SourceURL},
		{"ambiguous v1 base is default", Provider{BaseURL: "https://vyceai.com/v1"}, APIResponses, SourceDefault},
		{"empty base is default", Provider{}, APIResponses, SourceDefault},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api, source := tc.provider.WireAPISource()
			if api != tc.api || source != tc.source {
				t.Fatalf("WireAPISource() = (%s, %s), want (%s, %s)", api, source, tc.api, tc.source)
			}
			if got := tc.provider.WireAPI(); got != tc.api {
				t.Fatalf("WireAPI() = %s, want %s", got, tc.api)
			}
		})
	}
}
