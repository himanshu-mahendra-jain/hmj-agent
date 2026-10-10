package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hmj-agent/internal/config"
)

func TestProbeModelWireAPIs(t *testing.T) {
	// 1. Mock server handling responses, completions, and anthropic
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"delta\":\"pong\"}\n\n")

		case "/chat/completions":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n")

		case "/v1/messages":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("anthropic-version") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n")

		case "/auth-err/responses":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, "{\"error\":{\"message\":\"invalid api key provided\"}}")

		case "/rate-limit/responses":
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, "{\"error\":{\"message\":\"quota exceeded for current period\"}}")

		case "/slow/responses":
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"resp_prov": {
				BaseURL: server.URL,
				API:     config.APIResponses,
				Models:  []config.Model{{ID: "m-resp", Name: "Model Responses"}},
			},
			"comp_prov": {
				BaseURL: server.URL,
				API:     config.APICompletions,
				Models:  []config.Model{{ID: "m-comp"}},
			},
			"anth_prov": {
				BaseURL: server.URL,
				API:     config.APIAnthropic,
				APIKey:  "test-key",
				Models:  []config.Model{{ID: "m-anth"}},
			},
			"auth_prov": {
				BaseURL: server.URL + "/auth-err",
				API:     config.APIResponses,
				Models:  []config.Model{{ID: "m-auth"}},
			},
			"rate_prov": {
				BaseURL: server.URL + "/rate-limit",
				API:     config.APIResponses,
				Models:  []config.Model{{ID: "m-rate"}},
			},
		},
	}

	client := NewClient(cfg)
	ctx := context.Background()

	// Probe responses
	res1 := client.ProbeModel(ctx, config.ModelRef{Provider: "resp_prov", Model: "m-resp"})
	if res1.Status != StatusHealthy {
		t.Fatalf("expected healthy for resp_prov, got %s (details: %s)", res1.Status, res1.Details)
	}
	if res1.ModelName != "Model Responses" {
		t.Fatalf("expected display name 'Model Responses', got %q", res1.ModelName)
	}

	// Probe completions
	res2 := client.ProbeModel(ctx, config.ModelRef{Provider: "comp_prov", Model: "m-comp"})
	if res2.Status != StatusHealthy {
		t.Fatalf("expected healthy for comp_prov, got %s (details: %s)", res2.Status, res2.Details)
	}

	// Probe anthropic
	res3 := client.ProbeModel(ctx, config.ModelRef{Provider: "anth_prov", Model: "m-anth"})
	if res3.Status != StatusHealthy {
		t.Fatalf("expected healthy for anth_prov, got %s (details: %s)", res3.Status, res3.Details)
	}

	// Probe auth error
	res4 := client.ProbeModel(ctx, config.ModelRef{Provider: "auth_prov", Model: "m-auth"})
	if res4.Status != StatusAuthError {
		t.Fatalf("expected auth error for auth_prov, got %s (details: %s)", res4.Status, res4.Details)
	}

	// Probe rate limit
	res5 := client.ProbeModel(ctx, config.ModelRef{Provider: "rate_prov", Model: "m-rate"})
	if res5.Status != StatusRateLimited {
		t.Fatalf("expected rate limit for rate_prov, got %s (details: %s)", res5.Status, res5.Details)
	}

	// Test timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	cfgSlow := config.Config{
		Providers: map[string]config.Provider{
			"slow_prov": {
				BaseURL: server.URL + "/slow",
				API:     config.APIResponses,
				Models:  []config.Model{{ID: "m-slow"}},
			},
		},
	}
	clientSlow := NewClient(cfgSlow)
	resSlow := clientSlow.ProbeModel(timeoutCtx, config.ModelRef{Provider: "slow_prov", Model: "m-slow"})
	if resSlow.Status != StatusTimeout {
		t.Fatalf("expected timeout for slow_prov, got %s (details: %s)", resSlow.Status, resSlow.Details)
	}

	// Test CheckAllHealth concurrently
	allModels := cfg.AllModels()
	results := client.CheckAllHealth(ctx, allModels, nil)
	if len(results) != len(allModels) {
		t.Fatalf("expected %d results, got %d", len(allModels), len(results))
	}
}
