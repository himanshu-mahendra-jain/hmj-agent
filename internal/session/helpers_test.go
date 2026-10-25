package session_test

import (
	"hmj-agent/v2/internal/config"
)

func testModelRef() config.ModelRef {
	return config.ModelRef{Provider: "test", Model: "test-model"}
}
