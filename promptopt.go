package harness

// PromptOption changes how Session.Prompt and Session.PromptStream admit one input.
type PromptOption func(*promptSettings)

type promptSettings struct{ turnContext string }

func settingsOf(opts []PromptOption) (s promptSettings) {
	for _, o := range opts {
		o(&s)
	}
	return s
}

// WithTurnContext gives the model text in the turn that takes the input, as engine context after the input, as protocol.Input.TurnContext does; a non-empty text replaces that field. No event, history read, or later turn holds it, and a restart drops it from a queued input. A backend that owns the loop reads it once, with the input. Empty text adds nothing.
func WithTurnContext(text string) PromptOption {
	return func(s *promptSettings) { s.turnContext = text }
}
