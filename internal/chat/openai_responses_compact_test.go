package chat

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"hmj-agent/internal/config"
)

// compactedWindow is a reply's output from the compact endpoint: the encrypted compaction item
// and an item it kept.
const compactedWindow = `[{"type":"compaction","id":"cmp_1","encrypted_content":"ENC"},{"type":"message","role":"user","content":[{"type":"input_text","text":"kept prompt"}]}]`

// responsesServer answers /responses/compact with compact and /responses with a streamed "ok",
// recording each request's path and body.
type responsesServer struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func newResponsesServer(t *testing.T, compact http.HandlerFunc) *responsesServer {
	t.Helper()
	s := &responsesServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.bodies = append(s.bodies, string(body))
		s.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/compact") {
			compact(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

func responsesTestClient(url string, thinking string) *Client {
	c := NewClient(config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: url, APIKey: "k", API: "responses", Models: []config.Model{{ID: "m", Thinking: thinking}}},
	}})
	c.RetryConfig = RetryConfig{}
	return c
}

// TestResponsesCompactionSendsTheWindowAndReplaysItAsIs checks that the compact request carries
// the conversation without its system prompt, which goes as instructions, and that later requests
// send the system prompt, then the compacted window exactly as it came, then the new turn.
func TestResponsesCompactionSendsTheWindowAndReplaysItAsIs(t *testing.T) {
	srv := newResponsesServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_c","object":"response.compaction","output":%s,"usage":{"input_tokens":400000,"output_tokens":500}}`, compactedWindow)
	})
	client := responsesTestClient(srv.URL, "off")
	history := []Message{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: "question"},
		{Role: "assistant", Blocks: []Block{{Type: "reasoning", Signature: "rs_1", Data: "enc"}}, Content: "answer"},
	}

	compacted, err := client.Compact(context.Background(), testRef, history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := compacted.CompactionBlock(); b == nil || b.Type != BlockResponsesCompaction || b.Data != compactedWindow {
		t.Fatalf("compaction message = %+v, want the window as returned", compacted)
	}
	if srv.paths[0] != "/responses/compact" {
		t.Errorf("compact request went to %s", srv.paths[0])
	}
	var req struct {
		Model        string           `json:"model"`
		Instructions string           `json:"instructions"`
		Input        []map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(srv.bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "m" || req.Instructions != "SYSTEM" || len(req.Input) != 2 ||
		req.Input[0]["content"] != "question" || req.Input[1]["content"] != "answer" {
		t.Errorf("compact request = %s, want the model, the system prompt as instructions and the turn without reasoning", srv.bodies[0])
	}

	next := []Message{history[0], compacted, {Role: "user", Content: "next"}}
	if _, _, err := client.StreamChat(context.Background(), testRef, next, nil, func(string) {}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	want := `"input":[{"type":"message","role":"system","content":"SYSTEM"},` + compactedWindow[1:len(compactedWindow)-1] + `,{"type":"message","role":"user","content":"next"}]`
	if !strings.Contains(srv.bodies[1], want) {
		t.Errorf("turn request does not carry the window as returned:\n%s\nwant %s", srv.bodies[1], want)
	}
}

// TestResponsesCompactionMissingFallsBackToSummary checks that a provider without the compact
// endpoint gets a summary request instead.
func TestResponsesCompactionMissingFallsBackToSummary(t *testing.T) {
	srv := newResponsesServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	compacted, err := responsesTestClient(srv.URL, "").Compact(context.Background(), testRef, compactionTestHistory, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := compacted.CompactionBlock(); b == nil || b.Type != BlockSummary || b.Text != "ok" {
		t.Errorf("compaction message = %+v, want the written summary", compacted)
	}
	if len(srv.paths) != 2 || srv.paths[1] != "/responses" || !strings.Contains(srv.bodies[1], "Write a summary") {
		t.Errorf("want the compact request then a summary request, got %v", srv.paths)
	}
}

// TestResponsesCompactionWithoutCompactionItem checks that a window with no compaction item is
// reported as no summary.
func TestResponsesCompactionWithoutCompactionItem(t *testing.T) {
	srv := newResponsesServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"output":[{"type":"message","role":"user","content":"x"}]}`)
	})
	_, err := responsesTestClient(srv.URL, "").Compact(context.Background(), testRef, compactionTestHistory, nil)
	if err == nil || !strings.Contains(err.Error(), "no compaction item") {
		t.Errorf("err = %v", err)
	}
}
