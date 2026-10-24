package tools_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"hmj-agent/internal/tools"
)

// FuzzResolvePath ensures arbitrary paths (including traversal, NUL bytes,
// unicode, "~" forms, and absolute paths) never panic and always resolve to a
// clean absolute path.
func FuzzResolvePath(f *testing.F) {
	f.Add("ok.txt")
	f.Add("sub/dir/file.go")
	f.Add("../secret")
	f.Add("../../etc/passwd")
	f.Add("/etc/shadow")
	f.Add("sub/../../escape")
	f.Add("file\x00name")
	f.Add("")
	f.Add(" ")
	f.Add("\n\r\t")
	f.Add("./../../")
	f.Add("....//....//....//etc")
	f.Add("~")
	f.Add("~/notes.txt")
	f.Add("~user/notes.txt")

	f.Fuzz(func(t *testing.T, userPath string) {
		got, err := tools.ResolvePath(userPath)
		if err != nil {
			return
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("expected absolute path, input=%q, resolved=%q", userPath, got)
		}
		if filepath.Clean(got) != got {
			t.Fatalf("expected cleaned path, input=%q, resolved=%q", userPath, got)
		}
	})
}

// FuzzToolArguments verifies that arbitrary/malformed JSON strings generated
// by the model never cause tool executors to panic.
func FuzzToolArguments(f *testing.F) {
	f.Add("read_file", `{"path": "README.md"}`)
	f.Add("read_file", `{"path": "../../etc/passwd"}`)
	f.Add("read_file", `{"path": 12345}`)
	f.Add("read_file", `malformed json`)
	f.Add("edit_file", `{"path": "README.md", "old_string": "foo", "new_string": "bar"}`)
	f.Add("edit_file", `{"old_string": null}`)
	f.Add("list_files", `{}`)
	f.Add("list_files", `{"path": "../"}`)
	f.Add("fetch_url", `{"url": "http://127.0.0.1:8080"}`)
	f.Add("fetch_url", `{"url": "not-a-url"}`)
	f.Add("web_search", `{"query": "golang"}`)
	f.Add("web_search", `{"query": ""}`)

	fuzzableTools := map[string]tools.Tool{
		"read_file":  &tools.ReadFileTool{},
		"edit_file":  &tools.EditFileTool{},
		"list_files": &tools.ListFilesTool{},
		"fetch_url":  &tools.FetchURLTool{},
		"web_search": &tools.WebSearchTool{ // offline: every backend refuses the connection at once
			DDGHTMLEndpoint: "http://127.0.0.1:1/", BraveEndpoint: "http://127.0.0.1:1/", BingEndpoint: "http://127.0.0.1:1/",
			DDGInstantEndpoint: "http://127.0.0.1:1/", WikipediaEndpoint: "http://127.0.0.1:1/",
		},
	}

	f.Fuzz(func(t *testing.T, toolName, argsJSON string) {
		tool, ok := fuzzableTools[toolName]
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		// Execute must never panic, regardless of input corruption
		_, _ = tool.Execute(ctx, argsJSON)
	})
}
