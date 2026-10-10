package tests

import (
	"hmj-agent/internal/config"
)

func mockAgentConfig(url string) config.Config {
	return config.Config{
		Providers: map[string]config.Provider{
			"mock": {
				BaseURL: url,
				API:     "responses",
				Models:  []config.Model{{ID: "mock-model"}},
			},
		},
	}
}

func testModelRef() config.ModelRef {
	return config.ModelRef{Provider: "test", Model: "test-model"}
}
