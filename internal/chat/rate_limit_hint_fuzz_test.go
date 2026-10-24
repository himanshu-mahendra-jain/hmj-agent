package chat_test

import (
	"testing"
	"time"

	"hmj-agent/internal/chat"
)

// FuzzParseRateLimitHint checks the retry hint is never negative and never exceeds a day,
// whatever the provider's error body says.
func FuzzParseRateLimitHint(f *testing.F) {
	f.Add("")
	f.Add("Please try again in 4m39s")
	f.Add("You have exceeded your quota, retry in 1h2m3s")
	f.Add("retry in 90 seconds")
	f.Add("try again in 2 hours and 15 mins")
	f.Add("retry after 999999999h")
	f.Add("retry in 99999999999999999999s")
	f.Add("retry in 1.5.5s")
	f.Add("retry in 3 months")
	f.Add("RETRY IN 10 MS")
	f.Add("rate limit: try in 5, 6m")

	f.Fuzz(func(t *testing.T, body string) {
		d := chat.ParseRateLimitHint(body)
		if d < 0 || d > 24*time.Hour {
			t.Fatalf("ParseRateLimitHint(%q) = %v, want 0..24h", body, d)
		}
	})
}
