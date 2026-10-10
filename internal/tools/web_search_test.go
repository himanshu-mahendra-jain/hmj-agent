package tools

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCheckPublicRedirect(t *testing.T) {
	tests := map[string]bool{ // target -> allowed
		"https://8.8.8.8/search":        true,
		"http://[2606:4700::1111]/":     true,
		"http://127.0.0.1:8080/admin":   false,
		"http://[::1]/":                 false,
		"http://10.1.2.3/":              false,
		"http://192.168.1.1/":           false,
		"http://169.254.169.254/latest": false, // cloud metadata
		"http://[::ffff:127.0.0.1]/":    false,
		"http://0.0.0.0/":               false,
		"file:///etc/passwd":            false,
	}
	for target, allowed := range tests {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		err = checkPublicRedirect(&http.Request{URL: u}, nil)
		if (err == nil) != allowed {
			t.Errorf("redirect to %s: err = %v, want allowed=%v", target, err, allowed)
		}
	}
}

// The search client refuses a redirect into the local machine.
func TestSearchClientRefusesLocalRedirect(t *testing.T) {
	hit := false
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer internal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/secret", http.StatusFound)
	}))
	defer redirector.Close()

	_, err := publicSearchClient().Get(redirector.URL)
	if err == nil || !strings.Contains(err.Error(), "non-public address") || hit {
		t.Errorf("err = %v, internal server hit = %v; want the redirect refused", err, hit)
	}
}
