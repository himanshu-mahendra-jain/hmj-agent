package session_test

import (
	"hmj-agent/internal/config"
)

func testModelRef() config.ModelRef {
	return config.ModelRef{Provider: "test", Model: "test-model"}
}
