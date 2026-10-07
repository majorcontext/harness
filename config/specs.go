package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// validSessionSync lists the accepted session_sync values. A typo must fail, because "volume" avoids a deadlock on some network volumes.
var validSessionSync = map[string]bool{"": true, "fsync": true, "volume": true}

func validateSessionSync(v string) error {
	if !validSessionSync[v] {
		return fmt.Errorf("session_sync: unknown value %q; valid values: \"\" (default), \"fsync\", \"volume\"", v)
	}
	return nil
}

// validMCPToolLoading lists the accepted mcp_tool_loading values. "auto" is global-only: the threshold measures the whole catalog.
var validMCPToolLoading = map[string]bool{"": true, "eager": true, "auto": true, "lazy": true}

var validMCPServerToolLoading = map[string]bool{"": true, "eager": true, "lazy": true}

// validateMCPToolLoading rejects an unknown mode and a negative threshold, which would defer every catalog.
func validateMCPToolLoading(mode string, threshold int) error {
	if !validMCPToolLoading[mode] {
		return fmt.Errorf("mcp_tool_loading: unknown value %q; valid values: \"\" (default), \"eager\", \"auto\", \"lazy\"", mode)
	}
	if threshold < 0 {
		return fmt.Errorf("mcp_tool_loading_threshold: must not be negative (got %d)", threshold)
	}
	return nil
}

// ProcessSpec configures one managed process. At most one of ReadyRegex, ReadyPort, and ReadyHTTP gates Start.
type ProcessSpec struct {
	// Command is the argv; Command[0] is resolved via PATH.
	Command []string `json:"command,omitempty"`
	// Dir is the working directory, relative to the WorkDir.
	Dir string `json:"dir,omitempty"`
	// Env is appended to the harness environment.
	Env []string `json:"env,omitempty"`
	// Ports lists the TCP ports the process listens on, as metadata only (1-65535).
	Ports []int `json:"ports,omitempty"`
	// ReadyRegex is an RE2 pattern; a log line that matches it marks the process ready.
	ReadyRegex string `json:"ready_regex,omitempty"`
	// ReadyPort is a TCP port; the process is ready when a dial to 127.0.0.1:<port> succeeds.
	ReadyPort int `json:"ready_port,omitempty"`
	// ReadyHTTP is a URL; the process is ready when a GET returns a non-5xx status.
	ReadyHTTP string `json:"ready_http,omitempty"`
	// ReadyTimeoutS bounds the ready wait in seconds; zero or less means 60.
	ReadyTimeoutS int `json:"ready_timeout_s,omitempty"`
}

// MCPServerSpec configures one MCP server. Exactly one of Command (stdio) and URL (Streamable HTTP) is set.
type MCPServerSpec struct {
	// Command is the argv of a stdio server.
	Command []string `json:"command,omitempty"`
	// Env is appended to the harness environment.
	Env []string `json:"env,omitempty"`
	// Dir is the working directory of a stdio server.
	Dir string `json:"dir,omitempty"`
	// URL is the endpoint of a Streamable HTTP server.
	URL string `json:"url,omitempty"`
	// Headers are sent on every request to a Streamable HTTP server.
	Headers map[string]string `json:"headers,omitempty"`
	// ConnectTimeoutS bounds a connect attempt in seconds; zero means 15, and a negative value is rejected.
	ConnectTimeoutS int `json:"connect_timeout_s,omitempty"`
	// ToolLoading overrides Config.MCPToolLoading for this server: "eager" or "lazy". "auto" is rejected here.
	ToolLoading string `json:"tool_loading,omitempty"`
}

// PluginSpec configures one plugin process. Name and Command are required.
type PluginSpec struct {
	// Name is the manifest name, the cache key, and the chain identity.
	Name string `json:"name"`
	// Command is the argv of the plugin process.
	Command []string `json:"command"`
	// Env is appended to the harness environment.
	Env []string `json:"env,omitempty"`
	// Dir is the working directory of the plugin.
	Dir string `json:"dir,omitempty"`
	// Config is passed to the plugin in InitializeParams.
	Config json.RawMessage `json:"config,omitempty"`
}

func validatePlugins(plugins []PluginSpec) error {
	seen := make(map[string]bool, len(plugins))
	for i, p := range plugins {
		if p.Name == "" {
			return fmt.Errorf("plugins[%d]: name is required", i)
		}
		if len(p.Command) == 0 {
			return fmt.Errorf("plugins[%d] (%s): command is required", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("plugins[%d]: duplicate plugin name %q", i, p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// validateMCPServers requires a name that is free of "__" and "mcp__", so mcp__<server>__<tool> decodes in one way, and exactly one of command and url.
func validateMCPServers(servers map[string]MCPServerSpec) error {
	for name, s := range servers {
		if name == "" {
			return fmt.Errorf("mcp_servers: server name is required (empty key)")
		}
		if strings.Contains(name, "__") {
			return fmt.Errorf("mcp_servers.%s: server name must not contain \"__\" (the namespaced tool name mcp__<server>__<tool> would not be uniquely decodable)", name)
		}
		if strings.HasPrefix(name, "mcp__") {
			return fmt.Errorf("mcp_servers.%s: server name must not start with \"mcp__\" (reserved for the tool-namespace prefix)", name)
		}
		hasCommand := len(s.Command) > 0
		hasURL := s.URL != ""
		switch {
		case !hasCommand && !hasURL:
			return fmt.Errorf("mcp_servers.%s: exactly one of command (stdio) or url (streamable HTTP) is required", name)
		case hasCommand && hasURL:
			return fmt.Errorf("mcp_servers.%s: command and url are mutually exclusive", name)
		}
		if s.ConnectTimeoutS < 0 {
			return fmt.Errorf("mcp_servers.%s: connect_timeout_s must not be negative (got %d)", name, s.ConnectTimeoutS)
		}
		if !validMCPServerToolLoading[s.ToolLoading] {
			if s.ToolLoading == "auto" {
				return fmt.Errorf("mcp_servers.%s: tool_loading %q is global-only (the threshold it selects measures the whole catalog); use \"eager\" or \"lazy\" here", name, s.ToolLoading)
			}
			return fmt.Errorf("mcp_servers.%s: tool_loading: unknown value %q; valid values: \"\" (inherit), \"eager\", \"lazy\"", name, s.ToolLoading)
		}
	}
	return nil
}

func validateProcesses(processes map[string]ProcessSpec) error {
	for name, p := range processes {
		if name == "" {
			return fmt.Errorf("processes: process name is required (empty key)")
		}
		if err := p.Validate(); err != nil {
			return fmt.Errorf("processes.%s: %w", name, err)
		}
	}
	return nil
}

// copyProcessSpec copies the slices of s, so a merged config never aliases a layer.
func copyProcessSpec(s ProcessSpec) ProcessSpec {
	if len(s.Command) > 0 {
		s.Command = append([]string(nil), s.Command...)
	}
	if len(s.Env) > 0 {
		s.Env = append([]string(nil), s.Env...)
	}
	if len(s.Ports) > 0 {
		s.Ports = append([]int(nil), s.Ports...)
	}
	return s
}

// copyMCPServerSpec copies the slices and map of s, so a merged config never aliases a layer.
func copyMCPServerSpec(s MCPServerSpec) MCPServerSpec {
	if len(s.Command) > 0 {
		s.Command = append([]string(nil), s.Command...)
	}
	if len(s.Env) > 0 {
		s.Env = append([]string(nil), s.Env...)
	}
	if len(s.Headers) > 0 {
		hm := make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			hm[k] = v
		}
		s.Headers = hm
	}
	return s
}
