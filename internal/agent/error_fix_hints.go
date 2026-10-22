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
		case http.StatusBadRequest:
			if chat.IsContextOverflow(err) {
				return "the conversation is over the model's context limit; start a new conversation with /new"
			}
		case http.StatusUnauthorized:
			return "check the provider apiKey / its environment variable"
		case http.StatusPaymentRequired:
			return "check the billing and credit balance of the provider account"
		case http.StatusForbidden:
			return "check the provider apiKey, and that its account and workspace may use this model"
		case http.StatusNotFound:
			return "check the model id and baseUrl"
		case http.StatusRequestEntityTooLarge:
			return "the request is too large for the provider; start a new conversation with /new"
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
