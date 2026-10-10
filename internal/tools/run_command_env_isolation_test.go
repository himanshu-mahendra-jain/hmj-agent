package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRegisteredEnvIsNotInherited checks that a variable registered as holding a key is
// stripped from a command's environment even when its name has no credential suffix.
func TestRegisteredEnvIsNotInherited(t *testing.T) {
	t.Setenv("HMJ_TEST_OPENROUTER_KEY", "canary-registered")
	t.Setenv("HMJ_TEST_PLAIN_SETTING", "kept")
	RegisterSensitiveEnv("HMJ_TEST_OPENROUTER_KEY", "")
	t.Cleanup(func() {
		sensitiveEnvMu.Lock()
		delete(sensitiveEnvNames, "HMJ_TEST_OPENROUTER_KEY")
		sensitiveEnvMu.Unlock()
	})

	out, err := (&RunCommandTool{}).Execute(context.Background(),
		`{"command":"echo key=$HMJ_TEST_OPENROUTER_KEY plain=$HMJ_TEST_PLAIN_SETTING"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "key= plain=kept" {
		t.Errorf("output = %q, want the key stripped and the plain setting kept", out)
	}
}

// TestCommandsCannotReadKeysFromParentEnviron checks that once HideEnvironFromCommands
// has run, a command cannot read the keys stripped from its own environment back out of
// its parent's /proc/<pid>/environ, while its own /proc/self stays readable. It runs in
// a child test process so the non-dumpable flag does not leak into other tests.
func TestCommandsCannotReadKeysFromParentEnviron(t *testing.T) {
	const canary = "sk-environ-canary-0123456789"
	if os.Getenv("HMJ_TEST_ENVIRON_CHILD") == "1" {
		if err := HideEnvironFromCommands(); err != nil {
			fmt.Println("prctl failed:", err)
			os.Exit(2)
		}
		out, err := (&RunCommandTool{}).Execute(context.Background(),
			`{"command":"tr '\\0' '\\n' </proc/$PPID/environ; cat /proc/self/environ >/dev/null && echo self-readable"}`)
		fmt.Println(out, err)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCommandsCannotReadKeysFromParentEnviron$")
	cmd.Env = append(os.Environ(), "HMJ_TEST_ENVIRON_CHILD=1", "HMJ_TEST_CANARY_API_KEY="+canary)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child test process: %v\n%s", err, out)
	}
	if strings.Contains(string(out), canary) {
		t.Errorf("a command read the stripped key from its parent's environ:\n%s", out)
	}
	if !strings.Contains(string(out), "self-readable") {
		t.Errorf("a command could not read its own /proc/self/environ:\n%s", out)
	}
}
