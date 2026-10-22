package chat

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"hmj-agent/internal/config"
)

// HealthStatus categorizes the health of a provider model endpoint.
type HealthStatus string

const (
	StatusHealthy     HealthStatus = "OK"
	StatusFailed      HealthStatus = "FAIL"
	StatusRateLimited HealthStatus = "RATE_LIMITED"
	StatusAuthError   HealthStatus = "AUTH_ERROR"
	StatusTimeout     HealthStatus = "TIMEOUT"
)

// Read budgets for health probe responses.
const (
	// maxHealthSuccessBodyBytes is how much of a successful probe response is read before it is closed.
	maxHealthSuccessBodyBytes = 512
	// maxHealthErrorBodyBytes is how much of a failed probe response is read for its error message.
	maxHealthErrorBodyBytes = 1024
)

// probeBudget bounds the health request to one model, once its protocol is known.
const probeBudget = 8 * time.Second

// ModelHealthResult captures the result of a single token-frugal health probe.
type ModelHealthResult struct {
	Ref        config.ModelRef
	ModelName  string
	WireAPI    string
	Status     HealthStatus
	StatusCode int
	Latency    time.Duration
	Details    string
	Error      error
}

// ProbeModel executes a fast, token-frugal health check against a single model.
// It bypasses workspace guidelines, README/AGENTS.md injection, tool definitions,
// and chat history, sending a minimal 1-token prompt with max_tokens: 1.
//
// Protocol detection runs first, outside the 8-second request budget, because probing an
// endpoint of unknown protocol can take up to three 5-second attempts.
func (c *Client) ProbeModel(ctx context.Context, ref config.ModelRef) (result ModelHealthResult) {
	provider, modelCfg, err := c.Config.ResolveModel(ref)
	if err != nil {
		return ModelHealthResult{
			Ref:     ref,
			Status:  StatusFailed,
			Details: err.Error(),
			Error:   err,
		}
	}

	wireAPI, _, detectErr := c.DetectWireAPI(ctx, ref.Provider, provider)
	result = ModelHealthResult{
		Ref:       ref,
		ModelName: modelCfg.DisplayName(),
		WireAPI:   wireAPI,
	}
	defer func() {
		// An auth failure during detection means the protocol is unknown; say so next to the
		// request's own error. A cancelled detection is not a finding.
		if detectErr != nil && !errors.Is(detectErr, context.Canceled) && !errors.Is(detectErr, context.DeadlineExceeded) && result.Status != StatusHealthy {
			result.Details = "protocol unknown: " + result.Details
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()

	url, reqBody, err := probeRequest(wireAPI, strings.TrimRight(provider.BaseURL, "/"), modelCfg)
	if err != nil {
		result.Status = StatusFailed
		result.Details = "marshal error: " + err.Error()
		result.Error = err
		return result
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		result.Status = StatusFailed
		result.Details = "request error: " + err.Error()
		result.Error = err
		return result
	}
	setProbeHeaders(req, wireAPI, config.ResolveAPIKey(provider.APIKey))

	start := time.Now()
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		result.Latency = time.Since(start)
		result.Status, result.Details = probeTransportError(ctx, err)
		result.Error = err
		return result
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		buf := make([]byte, maxHealthSuccessBodyBytes)
		_, _ = resp.Body.Read(buf)
		result.Latency = time.Since(start)
		result.Status = StatusHealthy
		result.StatusCode = resp.StatusCode
		result.Details = "healthy"
		return result
	}

	result.Latency = time.Since(start)
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxHealthErrorBodyBytes))
	result.Status = probeHTTPStatus(resp.StatusCode)
	result.StatusCode = resp.StatusCode
	result.Details = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, parseHealthErrorMessage(bodyBytes, resp.StatusCode))
	return result
}

// probeRequest builds the minimal one-token request for wireAPI. baseURL must already have
// its trailing slashes trimmed. It returns the endpoint URL and the JSON body.
func probeRequest(wireAPI, baseURL string, modelCfg config.Model) (url string, body []byte, err error) {
	switch wireAPI {
	case config.APICompletions:
		if strings.HasSuffix(baseURL, "/chat/completions") || strings.HasSuffix(baseURL, "/completions") {
			url = baseURL
		} else {
			url = baseURL + "/chat/completions"
		}
		body, err = json.Marshal(map[string]any{
			"model": modelCfg.ID,
			"messages": []map[string]any{
				{"role": "user", "content": "1"},
			},
			"stream":     true,
			"max_tokens": 1,
		})

	case config.APIAnthropic:
		if strings.HasSuffix(baseURL, "/messages") || strings.HasSuffix(baseURL, "/v1/messages") {
			url = baseURL
		} else {
			url = baseURL + "/v1/messages"
		}
		body, err = json.Marshal(map[string]any{
			"model": modelCfg.ID,
			"messages": []map[string]any{
				{
					"role": "user",
					"content": []map[string]any{
						{"type": "text", "text": "1"},
					},
				},
			},
			"max_tokens": 1,
			"stream":     true,
		})

	case config.APIGemini:
		// A non-streaming interaction with store false, so the probe is not kept by Google. The
		// low thinking level keeps the one-token answer from waiting on a default-depth think.
		url = geminiInteractionsURL(baseURL)
		body, err = json.Marshal(map[string]any{
			"model": geminiModelName(modelCfg.ID),
			"input": []map[string]any{
				{"type": "user_input", "content": []map[string]any{{"type": "text", "text": "1"}}},
			},
			"store": false,
			"generation_config": map[string]any{
				"max_output_tokens": 1,
				"thinking_level":    "low",
			},
		})

	default: // APIResponses
		if strings.HasSuffix(baseURL, "/responses") {
			url = baseURL
		} else {
			url = baseURL + "/responses"
		}
		body, err = json.Marshal(map[string]any{
			"model": modelCfg.ID,
			"input": []map[string]any{
				{"type": "message", "role": "user", "content": "1"},
			},
			"stream":            true,
			"max_output_tokens": 1,
		})
	}
	return url, body, err
}

// setProbeHeaders sets the headers a probe request needs for wireAPI. Credentials are sent
// only when apiKey is non-empty.
func setProbeHeaders(req *http.Request, wireAPI, apiKey string) {
	req.Header.Set("Content-Type", "application/json")

	switch wireAPI {
	case config.APIAnthropic:
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("anthropic-version", AnthropicVersion)
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
	case config.APIGemini:
		if apiKey != "" {
			req.Header.Set("x-goog-api-key", apiKey)
		}
	default:
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
}

// probeTransportError classifies a failed round trip as a timeout, a cancellation or a
// plain failure, returning the status and the details text.
func probeTransportError(ctx context.Context, err error) (HealthStatus, string) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return StatusTimeout, "timeout (> 8s)"
	case errors.Is(ctx.Err(), context.Canceled):
		return StatusFailed, "cancelled"
	default:
		return StatusFailed, err.Error()
	}
}

// probeHTTPStatus maps a non-2xx HTTP status to its health status.
func probeHTTPStatus(code int) HealthStatus {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return StatusAuthError
	case http.StatusTooManyRequests:
		return StatusRateLimited
	case http.StatusGatewayTimeout:
		return StatusTimeout
	default:
		return StatusFailed
	}
}

// CheckAllHealth probes all given models concurrently with a bounded worker pool.
func (c *Client) CheckAllHealth(ctx context.Context, refs []config.ModelRef, onProgress func(done, total int)) []ModelHealthResult {
	if len(refs) == 0 {
		return nil
	}

	results := make([]ModelHealthResult, len(refs))
	maxWorkers := min(len(refs), 10)
	sem := make(chan struct{}, maxWorkers)

	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for i, ref := range refs {
		wg.Add(1)
		go func(idx int, r config.ModelRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := c.ProbeModel(ctx, r)
			results[idx] = res

			mu.Lock()
			done++
			if onProgress != nil {
				onProgress(done, len(refs))
			}
			mu.Unlock()
		}(i, ref)
	}

	wg.Wait()
	return results
}

func parseHealthErrorMessage(raw []byte, statusCode int) string {
	if len(raw) == 0 {
		return http.StatusText(statusCode)
	}
	var data struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	var msg string
	if err := json.Unmarshal(raw, &data); err == nil {
		if data.Message != "" {
			msg = data.Message
		} else if data.Detail != "" {
			msg = data.Detail
		} else if m, ok := data.Error.(map[string]any); ok {
			if s, ok := m["message"].(string); ok && s != "" {
				msg = s
			}
		} else if s, ok := data.Error.(string); ok && s != "" {
			msg = s
		}
	}
	if msg == "" {
		msg = string(raw)
	}

	msg = strings.TrimSpace(msg)
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	for strings.Contains(msg, "  ") {
		msg = strings.ReplaceAll(msg, "  ", " ")
	}

	runes := []rune(msg)
	if len(runes) > 55 {
		msg = string(runes[:52]) + "..."
	}
	return msg
}
