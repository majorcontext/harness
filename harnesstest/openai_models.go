package harnesstest

import (
	"encoding/json"
	"net/http"
)

const codexModelsPath = "/backend-api/codex/models"

// CodexModel is one entry of the model catalog that OpenAIOptions.Models serves.
type CodexModel struct {
	Slug, DisplayName, Description string
	// Visibility is "list", "hide", or "none". Empty: "list".
	Visibility string
	// Priority orders the catalog, lowest first.
	Priority int
	// Efforts are the reasoning levels the model supports, in the words of the backend.
	Efforts []string
	// ResponsesLite is the use_responses_lite flag of the model: the backend takes no hosted tool for it.
	ResponsesLite bool
}

// ModelRequests counts the requests for the model catalog.
func (o *OpenAI) ModelRequests() int {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return o.modelRequests
}

func (o *OpenAI) serveModels(w http.ResponseWriter, r *http.Request) {
	o.wmu.Lock()
	o.modelRequests++
	o.wmu.Unlock()
	if r.Method != http.MethodGet || r.URL.Query().Get("client_version") == "" {
		o.fault("model catalog request: method %s, client_version %q", r.Method, r.URL.Query().Get("client_version"))
		writeError(w, http.StatusBadRequest, "harnesstest: bad model catalog request")
		return
	}
	if o.opts.ModelsStatus != 0 {
		writeError(w, o.opts.ModelsStatus, "harnesstest: model catalog refused")
		return
	}
	models := make([]map[string]any, 0, len(o.opts.Models))
	for _, m := range o.opts.Models {
		levels := make([]map[string]any, 0, len(m.Efforts))
		for _, e := range m.Efforts {
			levels = append(levels, map[string]any{"effort": e, "description": e})
		}
		visibility := m.Visibility
		if visibility == "" {
			visibility = "list"
		}
		models = append(models, map[string]any{
			"slug": m.Slug, "display_name": m.DisplayName, "description": m.Description,
			"visibility": visibility, "priority": m.Priority, "supported_in_api": true,
			"supported_reasoning_levels": levels, "use_responses_lite": m.ResponsesLite,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}
