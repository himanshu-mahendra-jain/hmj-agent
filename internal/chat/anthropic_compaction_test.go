package chat

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// anthropicCompactionStream is the streamed reply to an on-demand compaction request: the block
// arrives whole in content_block_start, and the tokens are reported per iteration.
const anthropicCompactionStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"compaction\",\"content\":\"SUMMARY\",\"signature\":\"SIG\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"compaction\"},\"usage\":{\"output_tokens\":0,\"iterations\":[{\"type\":\"compaction\",\"input_tokens\":12,\"cache_read_input_tokens\":400000,\"output_tokens\":300}]}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// recordingServer answers each request with the next of replies and records the bodies and the
// anthropic-beta headers.
type recordingServer struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []string
	betas   []string
	replies []func(http.ResponseWriter)
}

func newRecordingServer(t *testing.T, replies ...func(http.ResponseWriter)) *recordingServer {
	t.Helper()
	s := &recordingServer{replies: replies}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		s.betas = append(s.betas, r.Header.Get("anthropic-beta"))
		reply := s.replies[0]
		if len(s.replies) > 1 {
			s.replies = s.replies[1:]
		}
		s.mu.Unlock()
		reply(w)
	}))
	t.Cleanup(s.Close)
	return s
}

func streamReply(stream string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}
}

func errorReply(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

const anthropicTextStream = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n" + anthropicEndTurn

var compactionTestHistory = []Message{
	{Role: "system", Content: "SYSTEM"},
	{Role: "user", Content: "question"},
	{Role: "assistant", Content: "answer"},
}

// TestAnthropicCompactionIsTheTurnRequestPlusCompaction checks that the compaction request is the
// conversation's own request, byte for byte apart from the compaction parameter, so it reads the
// conversation's prompt cache, and that it carries the beta header that ordinary turns do not.
func TestAnthropicCompactionIsTheTurnRequestPlusCompaction(t *testing.T) {
	srv := newRecordingServer(t, streamReply(anthropicTextStream), streamReply(anthropicCompactionStream))
	client := anthropicTestClient(srv.URL, 0)
	if _, _, err := client.StreamChat(context.Background(), testRef, compactionTestHistory, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	compacted, err := client.Compact(context.Background(), testRef, compactionTestHistory, nil)
	if err != nil {
		t.Fatal(err)
	}

	if b := compacted.CompactionBlock(); b == nil || *b != (Block{Type: BlockAnthropicCompaction, Text: "SUMMARY", Signature: "SIG"}) {
		t.Errorf("compaction message = %+v", compacted)
	}
	var turn, compaction map[string]any
	if err := json.Unmarshal([]byte(srv.bodies[0]), &turn); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(srv.bodies[1]), &compaction); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compaction["compaction"], map[string]any{"type": "summarize"}) {
		t.Errorf("compaction parameter = %v", compaction["compaction"])
	}
	delete(compaction, "compaction")
	if !reflect.DeepEqual(turn, compaction) {
		t.Errorf("the compaction request differs from the turn's request:\nturn:       %s\ncompaction: %s", srv.bodies[0], srv.bodies[1])
	}
	if srv.betas[0] != "" || srv.betas[1] != anthropicCompactionBeta {
		t.Errorf("anthropic-beta headers = %q, want none on the turn and %q on the compaction", srv.betas, anthropicCompactionBeta)
	}
}

// TestAnthropicCompactionBlockIsSentBackFirst checks that later requests start with the block
// exactly as the API returned it, with the beta header.
func TestAnthropicCompactionBlockIsSentBackFirst(t *testing.T) {
	srv := newRecordingServer(t, streamReply(anthropicTextStream))
	history := []Message{
		{Role: "system", Content: "SYSTEM"},
		{Role: "assistant", Blocks: []Block{{Type: BlockAnthropicCompaction, Text: "SUMMARY", Signature: "SIG"}}},
		{Role: "user", Content: "next"},
	}
	if _, _, err := anthropicTestClient(srv.URL, 0).StreamChat(context.Background(), testRef, history, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if want := `"messages":[{"role":"assistant","content":[{"type":"compaction","content":"SUMMARY","signature":"SIG"}]},`; !strings.Contains(srv.bodies[0], want) {
		t.Errorf("request does not start with the block as returned:\n%s", srv.bodies[0])
	}
	if srv.betas[0] != anthropicCompactionBeta {
		t.Errorf("anthropic-beta = %q, want %q", srv.betas[0], anthropicCompactionBeta)
	}
}

// TestAnthropicCompactionWithoutSummary checks that a compaction request that ends without a
// block is reported with its stop reason, and that a transient error is retried.
func TestAnthropicCompactionWithoutSummary(t *testing.T) {
	cutOff := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":0}}\n\n"
	srv := newRecordingServer(t,
		errorReply(529, `{"type":"error","error":{"type":"overloaded_error","message":"compaction_unavailable"}}`),
		streamReply(cutOff))
	_, err := anthropicTestClient(srv.URL, 2).Compact(context.Background(), testRef, compactionTestHistory, nil)
	if err == nil || !strings.Contains(err.Error(), "cut off") {
		t.Errorf("err = %v, want the summary reported as cut off", err)
	}
	if len(srv.bodies) != 2 {
		t.Errorf("%d requests, want the 529 retried once", len(srv.bodies))
	}
}

// TestAnthropicCompactionRejectedFallsBackToSummary checks that a provider that rejects the
// compaction request, as an Anthropic-compatible service may, gets a summary request instead.
func TestAnthropicCompactionRejectedFallsBackToSummary(t *testing.T) {
	srv := newRecordingServer(t,
		errorReply(400, `{"type":"error","error":{"type":"invalid_request_error","message":"compaction: Extra inputs are not permitted"}}`),
		streamReply("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"WRITTEN SUMMARY\"}}\n\n"+anthropicEndTurn))
	compacted, err := anthropicTestClient(srv.URL, 0).Compact(context.Background(), testRef, compactionTestHistory, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := compacted.CompactionBlock(); b == nil || b.Type != BlockSummary || b.Text != "WRITTEN SUMMARY" {
		t.Errorf("compaction message = %+v, want the written summary", compacted)
	}
	if len(srv.bodies) != 2 || strings.Contains(srv.bodies[1], `"compaction"`) || !strings.Contains(srv.bodies[1], "Write a summary") {
		t.Errorf("the second request should be the summary request:\n%s", strings.Join(srv.bodies, "\n"))
	}
}
