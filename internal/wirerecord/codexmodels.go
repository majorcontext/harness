package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/majorcontext/harness/internal/codexauth"
	"github.com/majorcontext/harness/internal/wirescrub"
)

const (
	codexModelsMaxString = 300
	// codexModelsURL is the model list of the Codex lane; its client_version matches the one the openai provider sends.
	codexModelsURL = "https://chatgpt.com/backend-api/codex/models?client_version=0.162.1"
)

// recordCodexModels records the model catalog of the ChatGPT Codex backend with the login that `harness login codex` stores.
func recordCodexModels(r *recorder, _ string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	url := codexModelsURL
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	src := codexauth.NewSource(codexauth.SourceOptions{Path: filepath.Join(home, ".harness", "auth", "codex.json")})
	hc := &http.Client{Transport: codexauth.NewTransport(src, nil, url), Timeout: time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	short, err := wirescrub.TruncateJSONL(body, codexModelsMaxString)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSpace(short), "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	return r.save("codexmodels.catalog.json", pretty.Bytes())
}
