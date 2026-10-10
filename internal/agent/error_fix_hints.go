package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/terminal/theme"
)

// RemediationHint suggests a fix for a common startup or request failure, or returns ""
// when none applies.
func RemediationHint(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, os.ErrNotExist) {
		return "create ~/.hmj/config.json (see config.example.json) or set HMJ_CONFIG"
	}
	if statusErr, ok := errors.AsType[*chat.StatusError](err); ok {
		switch statusErr.Code {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "check the provider apiKey / its environment variable"
		case http.StatusNotFound:
			return "check the model id and baseUrl"
		}
		return ""
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return "check baseUrl and network connectivity"
	}
	return ""
}

// PrintHint writes the remediation hint for err on its own line after the error. It only
// prints in accessibility mode, where the hint is spelled out as a [HINT] label.
func PrintHint(w io.Writer, err error) {
	if !theme.Accessibility() {
		return
	}
	if hint := RemediationHint(err); hint != "" {
		fmt.Fprintln(w, theme.Hint(hint))
	}
}
