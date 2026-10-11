package message

// EngineContext holds trusted runtime status. Only the runtime creates it.
//
// Transcoders render its sentinels and neutralize those sentinels in Text parts. Engine context is request-only, though canonical JSON retains the part.

type EngineContext struct {
	// Text is the rendered block body, e.g. "[engine: harness 0.1.0-dev ...]".
	// The producer already renders the "[name: ...]" shape; this part only
	// tags that string as engine-originated.
	Text string `json:"text"`
}

func (*EngineContext) partType() PartType { return PartEngineContext }

// PartEngineContext is the JSON discriminator for an EngineContext part.
const PartEngineContext PartType = "engine_context"
