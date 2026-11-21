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

// WithTurnContext gives the model text on each model call of the turn that takes the input, as engine context after the input. No event, history read, or later turn holds it, and a restart drops it from a queued input. A backend that owns the loop ignores it. Empty text adds nothing.
func WithTurnContext(text string) PromptOption {
	return func(s *promptSettings) { s.turnContext = text }
}
