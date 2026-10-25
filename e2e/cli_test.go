package e2e

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessSigintExitCleanly(t *testing.T) {
	bin := buildCLI(t)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"providers":{"p":{"baseUrl":"http://127.0.0.1","models":[{"id":"m"}]}}}`), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(cliEnv(), "HMJ_CONFIG="+cfgPath, "HMJ_SESSIONS="+filepath.Join(t.TempDir(), "sessions"))

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	defer stdinPipe.Close()

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start binary: %v", err)
	}

	// Read until prompt "> "
	buf := make([]byte, 1024)
	var output strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := stdoutPipe.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
			if strings.Contains(output.String(), "> ") {
				break
			}
		}
		if err != nil {
			break
		}
	}

	// Send SIGINT (simulating user pressing Ctrl+C at prompt)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("failed to send SIGINT: %v", err)
	}

	rem, err := io.ReadAll(stdoutPipe)
	if err != nil {
		t.Fatalf("failed to read stdout: %v", err)
	}
	output.Write(rem)

	err = cmd.Wait()
	exitCode := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}

	if exitCode != 130 {
		t.Fatalf("expected exit code 130 on SIGINT, got %d", exitCode)
	}

	// Verify output ends with a newline so bash prompt starts on a new line
	outStr := output.String()
	if !strings.HasSuffix(outStr, "\n") {
		t.Fatalf("expected output to end with newline on SIGINT, got: %q", outStr)
	}
}

func TestProcessCtrlDExitCleanly(t *testing.T) {
	bin := buildCLI(t)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"providers":{"p":{"baseUrl":"http://127.0.0.1","models":[{"id":"m"}]}}}`), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(cliEnv(), "HMJ_CONFIG="+cfgPath, "HMJ_SESSIONS="+filepath.Join(t.TempDir(), "sessions"))
	cmd.Stdin = strings.NewReader("") // immediate EOF (Ctrl+D)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected clean exit code 0 on EOF (Ctrl+D), got err: %v, out: %s", err, string(out))
	}
}

// TestProcessSignalsSaveSessionAndExit checks that each terminating signal saves the
// session file and exits with the conventional 128+N status.
func TestProcessSignalsSaveSessionAndExit(t *testing.T) {
	bin := buildCLI(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"providers":{"p":{"baseUrl":"http://127.0.0.1","models":[{"id":"m"}]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		sig  syscall.Signal
		want int
	}{
		{"SIGTERM", syscall.SIGTERM, 143},
		{"SIGHUP", syscall.SIGHUP, 129},
		{"SIGQUIT", syscall.SIGQUIT, 131},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessions := filepath.Join(t.TempDir(), "sessions")
			cmd := exec.Command(bin)
			cmd.Env = append(cliEnv(), "HMJ_CONFIG="+cfgPath, "HMJ_SESSIONS="+sessions)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}

			// The prompt means the signal handler is installed.
			var out strings.Builder
			buf := make([]byte, 1024)
			for !strings.Contains(out.String(), "> ") {
				n, err := stdout.Read(buf)
				out.Write(buf[:n])
				if err != nil {
					t.Fatalf("process ended before showing a prompt: %q", out.String())
				}
			}
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, stdout)
			code := 0
			if err := cmd.Wait(); err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = exitErr.ExitCode()
			}
			if code != tc.want {
				t.Errorf("exit code = %d, want %d", code, tc.want)
			}
			data, err := os.ReadFile(conversationFile(sessions, "1"))
			if err != nil {
				t.Fatalf("session file not saved: %v", err)
			}
			if !jsontext.Value(data).IsValid() {
				t.Errorf("session file is not valid JSON: %q", data)
			}
		})
	}
}

// SIGTERM during a turn must stop the running tool's process group (not orphan
// it), then save a session that includes the interrupted turn and exit 143.
func TestProcessSignalDuringToolKillsChildAndSaves(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t)

	pidFile := filepath.Join(dir, "child.pid")
	args, err := json.Marshal(map[string]string{"command": "echo $$ > " + pidFile + "; exec sleep 60"})
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(string(args))
	if err != nil {
		t.Fatal(err)
	}
	srv := mockAgentServer(t, []string{
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_sleep","name":"run_command"}}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_sleep","name":"run_command","arguments":` + string(quoted) + `}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	})
	defer srv.Close()

	cfgPath := filepath.Join(dir, "config.json")
	cfg := `{"providers":{"p":{"baseUrl":"` + srv.URL + `","api":"responses","models":[{"id":"m"}]}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "sessions")
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = append(cliEnv(), "HMJ_CONFIG="+cfgPath, "HMJ_SESSIONS="+sessions)
	cmd.Stdin = strings.NewReader("start the sleeper\n")
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var childPID int
	deadline := time.Now().Add(10 * time.Second)
	for childPID == 0 {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("tool never started")
		}
		if data, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(data), "\n") {
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &childPID); err != nil {
				t.Fatalf("bad pid file %q: %v", data, err)
			}
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	defer syscall.Kill(childPID, syscall.SIGKILL) // never leak the sleeper if an assertion fails

	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 143 {
		t.Fatalf("exit = %v, want code 143", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("shutdown took %v; the turn should stop promptly", elapsed)
	}

	// The child was killed with its process group, not left running.
	gone := false
	for range 100 {
		if syscall.Kill(childPID, 0) != nil {
			gone = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Errorf("tool process %d still running after shutdown", childPID)
	}

	data, err := os.ReadFile(conversationFile(sessions, "1"))
	if err != nil {
		t.Fatalf("session file not saved: %v", err)
	}
	if !strings.Contains(string(data), "start the sleeper") || !strings.Contains(string(data), "call_sleep") {
		t.Errorf("saved session is missing the interrupted turn: %s", data)
	}

	// The audit log next to the config records the interrupted command.
	auditLog, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil || !strings.Contains(string(auditLog), `"tool":"run_command"`) || !strings.Contains(string(auditLog), "exec sleep 60") {
		t.Errorf("audit log missing the tool run: %v, %s", err, auditLog)
	}
}

// A process killed outright mid-turn (out of memory, SIGKILL) has no chance to save, so the
// turn saves as it goes: the prompt and every finished step are on disk while the next one runs.
func TestProcessKilledMidTurnKeepsFinishedSteps(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t)

	// Every reply asks for the same command: its first run answers at once, its second sleeps.
	pidFile := filepath.Join(dir, "child.pid")
	countFile := filepath.Join(dir, "runs")
	command := "n=$(cat " + countFile + " 2>/dev/null || echo 0); echo $((n+1)) > " + countFile +
		"; if [ $n -ge 1 ]; then echo $$ > " + pidFile + "; exec sleep 60; fi; echo first-step-finished"
	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(string(args))
	if err != nil {
		t.Fatal(err)
	}
	srv := mockAgentServer(t, []string{
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_run","name":"run_command","arguments":` + string(quoted) + `}}`,
		`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	})
	defer srv.Close()

	cfgPath := filepath.Join(dir, "config.json")
	cfg := `{"providers":{"p":{"baseUrl":"` + srv.URL + `","api":"responses","models":[{"id":"m"}]}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "sessions")
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = append(cliEnv(), "HMJ_CONFIG="+cfgPath, "HMJ_SESSIONS="+sessions)
	cmd.Stdin = strings.NewReader("run it twice\n")
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var childPID int
	deadline := time.Now().Add(10 * time.Second)
	for childPID == 0 {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("second step never started")
		}
		if data, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(data), "\n") {
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &childPID); err != nil {
				t.Fatalf("bad pid file %q: %v", data, err)
			}
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	// A killed hmj cannot stop its tool's process group, so the test does.
	defer syscall.Kill(-childPID, syscall.SIGKILL)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	data, err := os.ReadFile(conversationFile(sessions, "1"))
	if err != nil {
		t.Fatalf("conversation not saved during the turn: %v", err)
	}
	if !strings.Contains(string(data), "run it twice") || !strings.Contains(string(data), "first-step-finished") {
		t.Errorf("saved conversation is missing the prompt or the finished step: %s", data)
	}
}
