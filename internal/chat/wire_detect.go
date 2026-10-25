package chat

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"hmj-agent/v2/internal/config"
	"hmj-agent/v2/internal/logging"
)

// SourceProbed is the wire source reported when the protocol was found by probing the endpoint.
const SourceProbed = "probed"

// probeTimeout bounds one probe request.
const probeTimeout = 5 * time.Second

// maxProbeBodyBytes is how much of a probe response body is read for classification.
const maxProbeBodyBytes = 4096

// protocolMemory remembers detection results for this process. A found protocol is also
// written to the config file as the provider's "api", so later runs need no probe. A base
// URL that matched nothing is remembered only in memory, with the auth error when there was
// one, so a turn does not probe it again; the endpoint may answer differently next run.
type protocolMemory struct {
	mu       sync.Mutex
	found    map[string]string
	defaults map[string]error
	// keyMu serialises detection per base URL, so models of one provider probe it once.
	keyMu map[string]*sync.Mutex
}

// protocolMemoryInit guards lazy creation of Client.protocols.
var protocolMemoryInit sync.Mutex

// protocolMemoryFor returns the client's detection memory, creating it on first use.
func (c *Client) protocolMemoryFor() *protocolMemory {
	protocolMemoryInit.Lock()
	defer protocolMemoryInit.Unlock()
	if c.protocols == nil {
		c.protocols = &protocolMemory{
			found:    map[string]string{},
			defaults: map[string]error{},
			keyMu:    map[string]*sync.Mutex{},
		}
	}
	return c.protocols
}

// protocolKey normalises a base URL for use as a memory key.
func protocolKey(baseURL string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
}

// lookup returns the protocol found for key in this process.
func (pm *protocolMemory) lookup(key string) (string, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	api, ok := pm.found[key]
	return api, ok
}

// lookupDefault reports whether key is remembered as matching nothing, with its auth error.
func (pm *protocolMemory) lookupDefault(key string) (error, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	err, ok := pm.defaults[key]
	return err, ok
}

// store remembers api for key.
func (pm *protocolMemory) store(key, api string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.found[key] = api
	delete(pm.defaults, key)
}

// storeDefault remembers that key matched nothing.
func (pm *protocolMemory) storeDefault(key string, err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.defaults[key] = err
}

// keyLock returns the detection mutex for key.
func (pm *protocolMemory) keyLock(key string) *sync.Mutex {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	m, ok := pm.keyMu[key]
	if !ok {
		m = &sync.Mutex{}
		pm.keyMu[key] = m
	}
	return m
}

// probeOutcome is what one probe request tells about its route.
type probeOutcome int

const (
	probeUnknown probeOutcome = iota // no usable evidence (200, HTML, 5xx, transport error)
	probeAbsent                      // 404: the route does not exist
	probeMatch                       // the route exists and speaks the probed protocol
	probeAuth                        // 401/403 with no recognisable body: auth gate, shape unknown
)

// probeCandidate is one route to try: the protocol it would mean and the URL to POST {} to.
type probeCandidate struct {
	api string
	url string
}

// probeCandidates lists the routes to probe for a base URL, in the order they are tried:
// Responses, then Anthropic Messages, then Chat Completions, then Gemini Interactions. Each
// URL is the one the protocol's request builder would use, so a match is a URL requests use.
func probeCandidates(baseURL string) []probeCandidate {
	base := strings.TrimRight(baseURL, "/")
	responses := base + "/responses"
	if strings.HasSuffix(base, "/responses") {
		responses = base
	}
	completions := base + "/chat/completions"
	if strings.HasSuffix(base, "/chat/completions") {
		completions = base
	}
	return []probeCandidate{
		{api: config.APIResponses, url: responses},
		{api: config.APIAnthropic, url: anthropicMessagesURL(base)},
		{api: config.APICompletions, url: completions},
		{api: config.APIGemini, url: geminiInteractionsURL(base)},
	}
}

// classifyProbe decides what a probe response says about the route for api. Only a JSON
// response can match; a 404 is always absent. OpenAI-style and FastAPI-style errors prove an
// OpenAI-compatible route (responses or completions), an Anthropic-style error proves the
// Messages route, and a Google-style error proves the Interactions route. A 401 or 403 with
// no recognisable body is an auth gate, not evidence.
func classifyProbe(api string, status int, contentType string, body []byte) probeOutcome {
	if status == http.StatusNotFound {
		return probeAbsent
	}
	shape := errorShape(body)
	if !strings.Contains(strings.ToLower(contentType), "json") {
		shape = ""
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusUnprocessableEntity:
		switch {
		case shape == "anthropic" && api == config.APIAnthropic:
			return probeMatch
		case (shape == "openai" || shape == "fastapi") && (api == config.APIResponses || api == config.APICompletions):
			return probeMatch
		case shape == "google" && api == config.APIGemini:
			return probeMatch
		}
	}
	if (status == http.StatusUnauthorized || status == http.StatusForbidden) && shape == "" {
		return probeAuth
	}
	return probeUnknown
}

// errorShape names the error envelope in body: "anthropic" for {"type":"error","error":{"type",
// "message"}}, "google" for {"error":{"code":<number>,"message","status"}} (Google also wraps
// it in a one-element array), "openai" for {"error":{"message",...}}, "fastapi" for
// {"detail":...}, or "".
func errorShape(body []byte) string {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) > 0 && body[0] == '[' {
		var items []jsontext.Value
		if err := json.Unmarshal(body, &items); err != nil || len(items) == 0 {
			return ""
		}
		body = items[0]
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return ""
	}
	if _, ok := data["detail"]; ok {
		return "fastapi"
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    *string        `json:"type"`
			Message *string        `json:"message"`
			Code    jsontext.Value `json:"code"`
			Status  *string        `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Error.Message == nil {
		return ""
	}
	if envelope.Type == "error" && envelope.Error.Type != nil {
		return "anthropic"
	}
	if envelope.Error.Status != nil || isJSONNumber(envelope.Error.Code) {
		return "google"
	}
	return "openai"
}

// isJSONNumber reports whether raw holds a JSON number.
func isJSONNumber(raw jsontext.Value) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && (s[0] == '-' || (s[0] >= '0' && s[0] <= '9'))
}

// probeRoute sends the empty-body probe to one candidate and classifies the answer. The
// returned error is set only for transport failures and cancellation.
func (c *Client) probeRoute(ctx context.Context, cand probeCandidate, apiKey string) (probeOutcome, int, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(pctx, http.MethodPost, cand.url, strings.NewReader("{}"))
	if err != nil {
		return probeUnknown, 0, err
	}
	setProbeHeaders(req, cand.api, apiKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return probeUnknown, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBodyBytes))
	if err != nil {
		return probeUnknown, resp.StatusCode, err
	}
	return classifyProbe(cand.api, resp.StatusCode, resp.Header.Get("Content-Type"), body), resp.StatusCode, nil
}

// DetectWireAPI resolves the protocol for a provider. An explicit "api" or a base-URL rule
// answers at once with no request. Otherwise the endpoint is probed in the order Responses,
// Anthropic Messages, Chat Completions, Gemini Interactions, one 5-second attempt each. The
// first match is remembered for this process and written into the config file as the
// provider's "api", so no later run probes it; OnProtocolDetected, when set, is told.
//
// When nothing matches it returns responses with source "default", logs that once, and
// remembers the result for this process. When every probe was refused with 401 or 403 and no
// recognisable body, the error says the auth failed and the protocol is unknown; the caller
// still gets responses.
func (c *Client) DetectWireAPI(ctx context.Context, providerName string, provider config.Provider) (api, source string, err error) {
	api, source = provider.WireAPISource()
	if source != config.SourceDefault {
		return api, source, nil
	}

	pm := c.protocolMemoryFor()
	key := protocolKey(provider.BaseURL)
	if found, ok := pm.lookup(key); ok {
		return found, SourceProbed, nil
	}
	if err, ok := pm.lookupDefault(key); ok {
		return config.APIResponses, config.SourceDefault, err
	}
	lock := pm.keyLock(key)
	lock.Lock()
	defer lock.Unlock()
	if found, ok := pm.lookup(key); ok {
		return found, SourceProbed, nil
	}
	if err, ok := pm.lookupDefault(key); ok {
		return config.APIResponses, config.SourceDefault, err
	}

	apiKey := config.ResolveAPIKey(provider.APIKey)
	sawAuth := false
	for _, cand := range probeCandidates(provider.BaseURL) {
		outcome, status, probeErr := c.probeRoute(ctx, cand, apiKey)
		logging.L().Debug("protocol probe", "provider", providerName, "api", cand.api, "status", status, "outcome", outcome)
		if err := ctx.Err(); err != nil {
			return config.APIResponses, config.SourceDefault, err
		}
		if probeErr != nil {
			continue
		}
		switch outcome {
		case probeMatch:
			pm.store(key, cand.api)
			logging.L().Info("protocol detected", "provider", providerName, "api", cand.api)
			saved := c.saveDetectedAPI(providerName, cand.api)
			if c.OnProtocolDetected != nil {
				c.OnProtocolDetected(providerName, cand.api, saved)
			}
			return cand.api, SourceProbed, nil
		case probeAuth:
			sawAuth = true
		}
	}
	if sawAuth {
		err = fmt.Errorf("%s: auth failed, protocol unknown (HTTP 401/403 with no recognisable body)", providerName)
		logging.L().Warn("protocol detection", "provider", providerName, "error", err.Error())
	} else {
		logging.L().Info("protocol not detected, using responses", "provider", providerName)
	}
	pm.storeDefault(key, err)
	return config.APIResponses, config.SourceDefault, err
}

// saveDetectedAPI writes the detected protocol into the provider's entry of the config file,
// so later runs need no probe. It reports whether the file was written; a failure is logged,
// and the protocol is still used for this process.
func (c *Client) saveDetectedAPI(providerName, api string) bool {
	if c.ConfigPath == "" {
		return false
	}
	if err := config.SetProviderAPI(c.ConfigPath, providerName, api); err != nil {
		logging.L().Warn("detected protocol not saved to config", "provider", providerName, "api", api, "path", c.ConfigPath, "error", err.Error())
		return false
	}
	logging.L().Info("detected protocol saved to config", "provider", providerName, "api", api, "path", c.ConfigPath)
	return true
}
