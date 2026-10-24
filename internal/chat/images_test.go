package chat

import (
	"encoding/base64"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useImageDir points the image store at a fresh directory for the test.
func useImageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetImageDir(dir)
	t.Cleanup(func() { SetImageDir("") })
	return dir
}

// imageToolResult returns a transcript ending in a tool result that carries img.
func imageToolResult(img Image) []Message {
	return []Message{
		{Role: "user", Content: "look"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "read_file", Arguments: `{"path":"a.png"}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "a.png: PNG image, 2×1", Images: []Image{img}},
	}
}

// marshal returns v as JSON text.
func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestImageToolResultsPerProvider checks the JSON each protocol sends for a tool result with
// a picture, in the shape its API documents, and the note sent in its place where it cannot go.
func TestImageToolResultsPerProvider(t *testing.T) {
	useImageDir(t)
	data := []byte("\x89PNG fake picture bytes")
	img, err := StoreImage(data, "image/png", 2, 1, "a.png")
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	msgs := imageToolResult(img)

	_, anthropic := convertMessagesToAnthropic(msgs)
	if got, want := marshal(t, anthropic[len(anthropic)-1].Content[0]), `{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"a.png: PNG image, 2×1"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+b64+`"}}]}`; got != want {
		t.Errorf("Anthropic:\n got %s\nwant %s", got, want)
	}

	items := convertMessagesToInputItems(msgs)
	if got, want := marshal(t, items[len(items)-1]), `{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"a.png: PNG image, 2×1"},{"type":"input_image","image_url":"data:image/png;base64,`+b64+`"}]}`; got != want {
		t.Errorf("Responses:\n got %s\nwant %s", got, want)
	}

	_, steps := convertMessagesToGemini(msgs)
	if got, want := marshal(t, steps[len(steps)-1]), `{"type":"function_result","name":"read_file","call_id":"call_1","result":[{"type":"text","text":"a.png: PNG image, 2×1"},{"type":"image","data":"`+b64+`","mime_type":"image/png"}]}`; got != want {
		t.Errorf("Gemini:\n got %s\nwant %s", got, want)
	}

	completions := convertMessagesToCompletions(msgs)
	if got, want := completions[len(completions)-1].Content, "a.png: PNG image, 2×1\n[image a.png (2×1 image/png) not shown: Chat Completions tool results carry text only]"; got != want {
		t.Errorf("Chat Completions content = %q, want %q", got, want)
	}
}

// TestToolResultWithoutImagesIsUnchanged checks that a tool result without pictures is still
// sent as a plain string.
func TestToolResultWithoutImagesIsUnchanged(t *testing.T) {
	msgs := imageToolResult(Image{})
	msgs[2].Images = nil
	_, anthropic := convertMessagesToAnthropic(msgs)
	if got := marshal(t, anthropic[len(anthropic)-1].Content[0]); !strings.Contains(got, `"content":"a.png: PNG image, 2×1"`) {
		t.Errorf("Anthropic: %s", got)
	}
	items := convertMessagesToInputItems(msgs)
	if got := marshal(t, items[len(items)-1]); !strings.Contains(got, `"output":"a.png: PNG image, 2×1"`) {
		t.Errorf("Responses: %s", got)
	}
}

// TestMissingImageBecomesANote checks that a picture gone from the store is sent as a note
// naming it, so an old conversation still resumes.
func TestMissingImageBecomesANote(t *testing.T) {
	dir := useImageDir(t)
	img, err := StoreImage([]byte("gif"), "image/gif", 3, 4, "shot.gif")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, img.File)); err != nil {
		t.Fatal(err)
	}
	_, anthropic := convertMessagesToAnthropic(imageToolResult(img))
	if got := marshal(t, anthropic[len(anthropic)-1].Content[0]); !strings.Contains(got, `{"type":"text","text":"[image shot.gif (3×4 image/gif) not shown: its file is gone from the image store]"}`) {
		t.Errorf("Anthropic: %s", got)
	}
}

// TestStoreImage checks the store: a picture is named by its content, stored once, refused
// without a store or with an unknown type, and the oldest go past the cap.
func TestStoreImage(t *testing.T) {
	if _, err := StoreImage([]byte("x"), "image/png", 1, 1, ""); err == nil {
		t.Error("stored without a store")
	}
	dir := useImageDir(t)
	a, err := StoreImage([]byte("same"), "image/jpeg", 1, 1, "a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := StoreImage([]byte("same"), "image/jpeg", 1, 1, "b.jpg")
	if a.File != b.File || !strings.HasSuffix(a.File, ".jpg") || len(a.File) != 64+4 {
		t.Errorf("names %q and %q, want one SHA-256 name", a.File, b.File)
	}
	if _, err := StoreImage([]byte("x"), "image/bmp", 1, 1, ""); err == nil {
		t.Error("stored an unsupported type")
	}
	for i := range 3 {
		os.WriteFile(filepath.Join(dir, "old"+string(rune('0'+i))+".png"), []byte("x"), 0o600)
	}
	pruneImagesTo(t, dir, 2)
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("%d files after pruning to 2", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, a.File)); err != nil {
		t.Errorf("the kept file was pruned: %v", err)
	}
}

// pruneImagesTo prunes dir down to n files, keeping the first file stored in it.
func pruneImagesTo(t *testing.T, dir string, n int) {
	t.Helper()
	old := maxStoredImages
	maxStoredImages = n
	defer func() { maxStoredImages = old }()
	entries, _ := os.ReadDir(dir)
	var keep string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jpg") {
			keep = e.Name()
		}
	}
	pruneImages(dir, keep)
}

func TestRemoveIfPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tmp")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeIfPresent(path); err != nil {
		t.Errorf("removing a file: %v", err)
	}
	if err := removeIfPresent(path); err != nil {
		t.Errorf("removing a file already gone: %v", err)
	}
	if err := removeIfPresent(filepath.Dir(path)); err != nil {
		t.Errorf("removing an empty directory: %v", err)
	}
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeIfPresent(full); err == nil {
		t.Error("removing a directory that is not empty returned no error")
	}
}
