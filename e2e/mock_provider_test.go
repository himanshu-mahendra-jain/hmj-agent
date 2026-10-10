package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// firstPromptIn returns the first line of the user's first prompt quoted in a title request, or
// "untitled" when the request does not quote one.
func firstPromptIn(body string) string {
	const lead = "First message from the user:\n"
	var find func(v any) string
	find = func(v any) string {
		switch x := v.(type) {
		case string:
			if _, after, ok := strings.Cut(x, lead); ok {
				first, _, _ := strings.Cut(after, "\n")
				return first
			}
		case []any:
			for _, e := range x {
				if s := find(e); s != "" {
					return s
				}
			}
		case map[string]any:
			for _, e := range x {
				if s := find(e); s != "" {
					return s
				}
			}
		}
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err == nil {
		if s := find(v); s != "" {
			return s
		}
	}
	return "untitled"
}

// reply writes one model response. Each request hmj makes (one per agent step) takes the
// next queued reply; an empty queue answers "ok".
type reply func(w http.ResponseWriter, r *http.Request)

// provider is a mock OpenAI Responses API endpoint (POST /responses) with scripted replies.
type provider struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	queue    []reply
	requests []string // request bodies, in arrival order
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{t: t}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("mock provider: reading request: %v", err)
		}
		// The title request that follows a conversation's first turn is answered here, so it
		// takes no scripted reply and is not counted among the requests the tests script.
		if strings.Contains(string(body), "Reply with the title only") {
			say("Mock: "+firstPromptIn(string(body)))(w, r)
			return
		}
		p.mu.Lock()
		p.requests = append(p.requests, string(body))
		next := say("ok")
		if len(p.queue) > 0 {
			next, p.queue = p.queue[0], p.queue[1:]
		}
		p.mu.Unlock()
		next(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// URL is the provider's base URL for the config file.
func (p *provider) URL() string { return p.srv.URL }

// Queue appends replies for the next requests.
func (p *provider) Queue(replies ...reply) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, replies...)
}

// Requests returns the request bodies received so far.
func (p *provider) Requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

// LastRequest returns the most recent request body.
func (p *provider) LastRequest() string {
	p.t.Helper()
	reqs := p.Requests()
	if len(reqs) == 0 {
		p.t.Fatal("mock provider received no requests")
	}
	return reqs[len(reqs)-1]
}

// config returns a config with this provider named "mock" offering the given models.
func (p *provider) config(models ...string) string {
	if len(models) == 0 {
		models = []string{"model-a"}
	}
	var ms []string
	for _, m := range models {
		ms = append(ms, fmt.Sprintf(`{"id":%q}`, m))
	}
	return fmt.Sprintf(`{"providers":{"mock":{"baseUrl":%q,"api":"responses","models":[%s]}}}`,
		p.URL(), strings.Join(ms, ","))
}

// sse writes one server-sent event and flushes it.
func sse(w http.ResponseWriter, event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func startStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
}

func completed(w http.ResponseWriter) {
	sse(w, "response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"status": "completed",
			"usage":  map[string]int{"input_tokens": 100, "output_tokens": 10, "total_tokens": 110},
		},
	})
}

// say replies with text.
func say(text string) reply {
	return func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sse(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": text})
		completed(w)
	}
}

// callTool replies with one function call to the named tool.
func callTool(name string, args map[string]any) reply {
	return func(w http.ResponseWriter, r *http.Request) {
		argJSON, err := json.Marshal(args)
		if err != nil {
			panic(err)
		}
		id := "call_" + name
		startStream(w)
		sse(w, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"id": "item_" + name, "type": "function_call", "call_id": id, "name": name},
		})
		sse(w, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]any{"id": "item_" + name, "type": "function_call", "call_id": id, "name": name, "arguments": string(argJSON)},
		})
		completed(w)
	}
}

// streamForever sends word on its own line every interval until the client goes away,
// for halt tests. Whole lines, because hmj renders streamed text a line at a time.
func streamForever(word string, interval time.Duration) reply {
	return func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		for {
			sse(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": word + "\n"})
			select {
			case <-r.Context().Done():
				return
			case <-time.After(interval):
			}
		}
	}
}

// sayDeltas replies with text streamed as the given deltas, in order, so a test can split a
// line, a table or an escape sequence across chunks.
func sayDeltas(deltas ...string) reply {
	return func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		for _, d := range deltas {
			sse(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": d})
		}
		completed(w)
	}
}

// sayAfter streams before, waits until release is closed (or the client goes away), then
// streams after and completes. It lets a test act, such as resizing the terminal, while a
// reply is half streamed.
func sayAfter(before string, release <-chan struct{}, after string) reply {
	return func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sse(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": before})
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		sse(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": after})
		completed(w)
	}
}

// hang accepts the request and never answers, until the client goes away: a model that is
// still thinking, for halt tests.
func hang() reply {
	return func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}
}

// httpError replies with a non-streaming error status.
func httpError(status int, body string) reply {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}
