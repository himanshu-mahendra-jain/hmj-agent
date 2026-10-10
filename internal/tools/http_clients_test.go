package tools

import (
	"net/http"
	"strings"
	"testing"
)

func TestPooledClientsAreSingletons(t *testing.T) {
	if publicSearchClient() != publicSearchClient() {
		t.Error("publicSearchClient should return the same instance")
	}
	if fetchClient() != fetchClient() {
		t.Error("fetchClient should return the same instance")
	}
	if publicSearchClient() == fetchClient() {
		t.Error("search and fetch clients must remain distinct")
	}
}

func TestFetchClientCheckRedirect(t *testing.T) {
	client := fetchClient()
	if client == nil || client.CheckRedirect == nil {
		t.Fatal("expected fetchClient with CheckRedirect configured")
	}

	// Non-HTTP scheme blocked
	badReq, err := http.NewRequest("GET", "ftp://example.com/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(badReq, nil); err == nil {
		t.Fatal("expected error on redirect to ftp scheme")
	}

	// Redirect limit reached
	httpReq, err := http.NewRequest("GET", "https://example.com/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	via10 := make([]*http.Request, 10)
	if err := client.CheckRedirect(httpReq, via10); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("expected redirect limit error, got: %v", err)
	}

	// Normal redirect allowed
	via2 := make([]*http.Request, 2)
	if err := client.CheckRedirect(httpReq, via2); err != nil {
		t.Fatalf("unexpected error on valid redirect: %v", err)
	}
}
