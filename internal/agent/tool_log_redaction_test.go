package agent

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/logging"
	"hmj-agent/internal/redact"
)

// A failed tool's error often repeats its arguments, as fetch_url's does with the full URL,
// so the diagnostic log must redact the error as well as the arguments.
func TestLogToolRedactsArgsAndError(t *testing.T) {
	const configuredKey = "proxy-key-0123456789abcdef" // matches no token format; masked only once registered
	const githubToken = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	redact.Register(configuredKey)

	logPath := filepath.Join(t.TempDir(), "hmj.log")
	closeLog, err := logging.Init(logPath)
	if err != nil {
		t.Fatalf("logging.Init: %v", err)
	}
	t.Cleanup(func() {
		if _, err := logging.Init(""); err != nil {
			t.Errorf("reset logger: %v", err)
		}
	})

	target := "https://api.example.com/x?access_token=" + githubToken + "&key=" + configuredKey
	args := fmt.Sprintf(`{"url":%q}`, target)
	execErr := fmt.Errorf("HTTP request failed: %w", &url.Error{Op: "Get", URL: target, Err: fmt.Errorf("connection refused")})
	logTool("fetch_url", args, 0, "", execErr)
	if err := closeLog(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, `"msg":"tool"`) || !strings.Contains(log, `"error":"HTTP request failed`) {
		t.Fatalf("expected a tool record with an error, got:\n%s", log)
	}
	for _, secret := range []string{configuredKey, githubToken} {
		if strings.Contains(log, secret) {
			t.Errorf("secret %q leaked into the log:\n%s", secret, log)
		}
	}
}
