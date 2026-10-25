package chat

import "hmj-agent/v2/internal/config"

// EffortLabel says what reasoning effort requests to model over provider carry: the configured
// level; "default" when none is set, leaving it to the provider; how Gemini narrows xhigh and
// max to high and Anthropic or Gemini map none and minimal; or that the level is not sent, with
// thinking off on a protocol that sends effort only alongside reasoning. A provider whose
// protocol hmj finds by probing is shown as configured.
func EffortLabel(provider config.Provider, model config.Model) string {
	if model.Effort == "" {
		return "default"
	}
	api, source := provider.WireAPISource()
	if source == config.SourceDefault {
		return model.Effort
	}
	switch {
	case api != config.APIAnthropic && !model.ThinkingEnabled():
		return "default (" + model.Effort + " not sent: thinking off)"
	case api == config.APIGemini && geminiThinkingLevel(model.Effort) != model.Effort:
		return geminiThinkingLevel(model.Effort) + " (from " + model.Effort + ")"
	case api == config.APIAnthropic && anthropicEffort(model.Effort) != model.Effort:
		return anthropicEffort(model.Effort) + " (from " + model.Effort + ")"
	}
	return model.Effort
}
