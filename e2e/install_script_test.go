package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease serves a repository with releases v1.0.0 (the latest) and v0.9.0, laid out as
// release.yml publishes them, each holding a stand-in hmj that prints its version. corrupt
// replaces v1.0.0's tarball after its sum is taken.
func fakeRelease(t *testing.T, corrupt bool) *httptest.Server {
	t.Helper()
	files := map[string][]byte{}
	for _, v := range []string{"1.0.0", "0.9.0"} {
		name := fmt.Sprintf("hmj-%s-linux-%s.tar.gz", v, runtime.GOARCH)
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		bin := fmt.Appendf(nil, "#!/bin/sh\necho 'HMJ Agent %s (abc1234)'\n", v)
		tw.WriteHeader(&tar.Header{Name: "hmj/hmj", Mode: 0o755, Size: int64(len(bin))})
		tw.Write(bin)
		tw.Close()
		gz.Close()
		files["/releases/download/v"+v+"/"+name] = buf.Bytes()
		sums := fmt.Sprintf("%x  %s\n", sha256.Sum256(buf.Bytes()), name)
		files["/releases/download/v"+v+"/SHA256SUMS.txt"] = []byte(sums)
		if corrupt && v == "1.0.0" {
			files["/releases/download/v"+v+"/"+name] = append(buf.Bytes(), 0)
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases/latest":
			http.Redirect(w, r, "/releases/tag/v1.0.0", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/releases/tag/"):
		case files[r.URL.Path] != nil:
			w.Write(files[r.URL.Path])
		default:
			http.NotFound(w, r)
		}
	}))
}

// runInstall runs install.sh against server, installing into dir, with extra environment.
func runInstall(t *testing.T, server *httptest.Server, dir string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "../install.sh")
	cmd.Env = append(os.Environ(), append([]string{"HMJ_REPO_URL=" + server.URL, "HMJ_INSTALL_DIR=" + dir}, env...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestInstallScript checks that install.sh installs the latest release, leaves it alone when it
// is already installed, moves to a pinned version, and refuses a download that does not match
// its sum without touching the installed binary.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("releases are built for linux/amd64 and linux/arm64 only")
	}
	for _, tool := range []string{"sh", "curl", "tar", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("install.sh needs %s", tool)
		}
	}
	dir := filepath.Join(t.TempDir(), "bin")
	server := fakeRelease(t, false)
	defer server.Close()

	steps := []struct {
		env  []string
		want string
	}{
		{nil, "Installed " + dir + "/hmj\nHMJ Agent 1.0.0 (abc1234)"},
		{nil, "hmj v1.0.0 is already installed"},
		{[]string{"HMJ_VERSION=0.9.0"}, "Updated " + dir + "/hmj from: HMJ Agent 1.0.0 (abc1234)\nHMJ Agent 0.9.0 (abc1234)"},
	}
	for _, s := range steps {
		out, err := runInstall(t, server, dir, s.env...)
		if err != nil || !strings.Contains(out, s.want) {
			t.Fatalf("install.sh %v: err %v, output %q, want it to contain %q", s.env, err, out, s.want)
		}
	}

	bad := fakeRelease(t, true)
	defer bad.Close()
	out, err := runInstall(t, bad, dir)
	if err == nil || !strings.Contains(out, "does not match its SHA-256 sum") {
		t.Errorf("corrupt download: err %v, output %q", err, out)
	}
	if got, _ := exec.Command(filepath.Join(dir, "hmj")).Output(); string(got) != "HMJ Agent 0.9.0 (abc1234)\n" {
		t.Errorf("installed hmj after a refused download prints %q, want the earlier version", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("install dir holds %d entries, want only hmj", len(entries))
	}
}
