package openai

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/codexauth"
	"github.com/majorcontext/harness/internal/modelmeta"
)

// maxModelList bounds the body of a model list.
const maxModelList = 1 << 20

// effortOrder lists the harness effort levels in display order.
var effortOrder = []string{"off", "minimal", "low", "medium", "high"}

// ModelsURL returns the URL of the model list of the ChatGPT Codex backend at base.
func ModelsURL(base string) string {
	if base == "" {
		base = defaultBaseURL
	}
	return strings.TrimRight(base, "/") + "/models?client_version=" + codexauth.ClientVersion
}

// ModelsStatusError reports that the model list answered with an HTTP status other than 200.
type ModelsStatusError struct{ Status int }

// Error implements error.
func (e *ModelsStatusError) Error() string {
	return fmt.Sprintf("openai: model list answered HTTP %d", e.Status)
}

// codexEntry is one model of the model list of the ChatGPT Codex backend.
type codexEntry struct {
	Slug                     string `json:"slug"`
	DisplayName              string `json:"display_name"`
	Description              string `json:"description"`
	Visibility               string `json:"visibility"`
	Priority                 int    `json:"priority"`
	SupportedReasoningLevels []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
}

// codexCatalog is the body of the model list of the ChatGPT Codex backend.
type codexCatalog struct {
	Models []codexEntry `json:"models"`
}

// ListModels fetches the model list of the ChatGPT Codex backend, lowest priority first. HTTPClient attaches the login.
// It lists the models that the backend shows in its picker, as the Codex CLI does. A model that lacks a name, a description,
// or reasoning levels takes those of the models.dev entry of its id.
func (c *Client) ListModels(ctx context.Context) ([]modelmeta.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsURL(c.BaseURL), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &ModelsStatusError{resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelList))
	if err != nil {
		return nil, err
	}
	var body codexCatalog
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("openai: model list is not JSON: %w", err)
	}
	return catalogModels(body), nil
}

// catalogModels maps the picker models of a catalog to modelmeta models, lowest priority first.
func catalogModels(body codexCatalog) []modelmeta.Model {
	shown := slices.DeleteFunc(slices.Clone(body.Models), func(e codexEntry) bool {
		return e.Visibility != "list" || e.Slug == ""
	})
	slices.SortStableFunc(shown, func(a, b codexEntry) int { return a.Priority - b.Priority })
	out := make([]modelmeta.Model, 0, len(shown))
	for _, e := range shown {
		m := modelmeta.Model{ID: e.Slug, Name: e.DisplayName, Description: e.Description, Efforts: harnessEfforts(e)}
		if facts, ok := modelmeta.OpenAIModel(e.Slug); ok {
			m.Name = cmp.Or(m.Name, facts.Name)
			m.Description = cmp.Or(m.Description, facts.Description)
			if len(m.Efforts) == 0 {
				m.Efforts = facts.Efforts
			}
		}
		out = append(out, m)
	}
	return out
}

// harnessEfforts returns the levels of e that harness knows, in display order. The backend word none is off.
func harnessEfforts(e codexEntry) []string {
	var out []string
	for _, level := range effortOrder {
		for _, l := range e.SupportedReasoningLevels {
			if l.Effort == level || (level == "off" && l.Effort == "none") {
				out = append(out, level)
				break
			}
		}
	}
	return out
}
