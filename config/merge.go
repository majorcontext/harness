package config

// merge returns base overlaid with the non-zero fields of over. The result aliases no map or slice of either input.
func merge(base, over *Config) *Config {
	out := *base
	out.Aliases = nil
	out.Providers = nil
	mergeScalars(&out, over)
	mergeDirs(&out, base, over)
	mergeAliases(&out, base, over)
	mergeProviders(&out, base, over)
	mergeSpecs(&out, base, over)
	return &out
}

func mergeScalars(out, over *Config) {
	if over.Model != "" {
		out.Model = over.Model
	}
	if over.SessionDir != "" {
		out.SessionDir = over.SessionDir
	}
	if over.Instructions != nil {
		out.Instructions = over.Instructions
	}
	if over.InstructionsPath != "" {
		out.InstructionsPath = over.InstructionsPath
	}
	if over.InstructionsMaxBytes != 0 {
		out.InstructionsMaxBytes = over.InstructionsMaxBytes
	}
	if over.GoalEvaluatorModel != "" {
		out.GoalEvaluatorModel = over.GoalEvaluatorModel
	}
	if over.MaxTaskDepth != 0 {
		out.MaxTaskDepth = over.MaxTaskDepth
	}
	if over.MaxConcurrentTasks != 0 {
		out.MaxConcurrentTasks = over.MaxConcurrentTasks
	}
	if over.MaxTreeTokens != 0 {
		out.MaxTreeTokens = over.MaxTreeTokens
	}
	if over.ModelTool != nil {
		out.ModelTool = over.ModelTool
	}
	if over.ContextWindowTokens != 0 {
		out.ContextWindowTokens = over.ContextWindowTokens
	}
	if over.PromptRetries != nil {
		out.PromptRetries = over.PromptRetries
	}
	if over.MaxTokensContinuations != nil {
		out.MaxTokensContinuations = over.MaxTokensContinuations
	}
	if over.StreamIdleTimeoutS != 0 {
		out.StreamIdleTimeoutS = over.StreamIdleTimeoutS
	}
	if over.CompactionThreshold != 0 {
		out.CompactionThreshold = over.CompactionThreshold
	}
	if over.CompactionKeepTurns != 0 {
		out.CompactionKeepTurns = over.CompactionKeepTurns
	}
	if over.SessionSync != "" {
		out.SessionSync = over.SessionSync
	}
	if over.MCPToolLoading != "" {
		out.MCPToolLoading = over.MCPToolLoading
	}
	if over.MCPToolLoadingThreshold != 0 {
		out.MCPToolLoadingThreshold = over.MCPToolLoadingThreshold
	}
}

func mergeDirs(out, base, over *Config) {
	// Arrays override wholesale: a non-empty project value replaces the user
	// value entirely; otherwise inherit. Copy so the merged config never
	// aliases either input's slice.
	src := out.SkillsDirs
	if len(over.SkillsDirs) > 0 {
		src = over.SkillsDirs
	}
	if len(src) > 0 {
		out.SkillsDirs = append([]string(nil), src...)
	}
	agentDefsSrc := out.AgentDefsDirs
	if len(over.AgentDefsDirs) > 0 {
		agentDefsSrc = over.AgentDefsDirs
	}
	if len(agentDefsSrc) > 0 {
		out.AgentDefsDirs = append([]string(nil), agentDefsSrc...)
	}
	commandsSrc := out.CommandsDirs
	if over.CommandsDirs != nil {
		commandsSrc = over.CommandsDirs
	}
	if commandsSrc != nil {
		out.CommandsDirs = append([]string{}, commandsSrc...)
	}
	// AppendSystemPrompt CONCATENATES, base first — the one additive slice
	// rule in this function. See the field's own doc comment for why a
	// project layer must not be able to drop a platform-supplied segment.
	// A fresh slice, so the merged config aliases neither input.
	if n := len(base.AppendSystemPrompt) + len(over.AppendSystemPrompt); n > 0 {
		segs := make([]string, 0, n)
		segs = append(segs, base.AppendSystemPrompt...)
		segs = append(segs, over.AppendSystemPrompt...)
		out.AppendSystemPrompt = segs
	}
}

func mergeAliases(out, base, over *Config) {
	if n := len(base.Aliases) + len(over.Aliases); n > 0 {
		m := make(map[string]string, n)
		for k, v := range base.Aliases {
			m[k] = v
		}
		for k, v := range over.Aliases {
			m[k] = v
		}
		out.Aliases = m
	}
}

func mergeProviders(out, base, over *Config) {
	n := len(base.Providers) + len(over.Providers)
	if n == 0 {
		return
	}
	m := make(map[string]Provider, n)
	for k, v := range base.Providers {
		m[k] = copyProvider(v)
	}
	for k, v := range over.Providers {
		if ex, ok := m[k]; ok {
			m[k] = overlayProvider(ex, v)
		} else {
			m[k] = copyProvider(v)
		}
	}
	out.Providers = m
}

func copyProvider(v Provider) Provider {
	if len(v.ExtraHeaders) > 0 {
		hm := make(map[string]string, len(v.ExtraHeaders))
		for hk, hv := range v.ExtraHeaders {
			hm[hk] = hv
		}
		v.ExtraHeaders = hm
	}
	if len(v.OmitResponseParams) > 0 {
		v.OmitResponseParams = append([]string(nil), v.OmitResponseParams...)
	}
	if len(v.ExtraArgs) > 0 {
		v.ExtraArgs = append([]string(nil), v.ExtraArgs...)
	}
	return v
}

func overlayProvider(ex, v Provider) Provider {
	if v.Type != "" {
		ex.Type = v.Type
	}
	if v.APIKeyEnv != "" {
		ex.APIKeyEnv = v.APIKeyEnv
	}
	if v.BaseURL != "" {
		ex.BaseURL = v.BaseURL
	}
	if v.Family != "" {
		ex.Family = v.Family
	}
	if v.CacheTTL != "" {
		ex.CacheTTL = v.CacheTTL
	}
	if v.ResponsesPath != "" {
		ex.ResponsesPath = v.ResponsesPath
	}
	if len(v.OmitResponseParams) > 0 {
		ex.OmitResponseParams = append([]string(nil), v.OmitResponseParams...)
	}
	if v.NoPromptCacheKey {
		ex.NoPromptCacheKey = true
	}
	if v.SanitizeToolSchemas {
		ex.SanitizeToolSchemas = true
	}
	if v.UseWebSocketTransport {
		ex.UseWebSocketTransport = true
	}
	if v.BinaryPath != "" {
		ex.BinaryPath = v.BinaryPath
	}
	if len(v.ExtraArgs) > 0 {
		ex.ExtraArgs = append([]string(nil), v.ExtraArgs...)
	}
	if v.PermissionMode != "" {
		ex.PermissionMode = v.PermissionMode
	}
	if v.SessionMirror {
		ex.SessionMirror = true
	}
	if n := len(ex.ExtraHeaders) + len(v.ExtraHeaders); n > 0 {
		hm := make(map[string]string, n)
		for hk, hv := range ex.ExtraHeaders {
			hm[hk] = hv
		}
		for hk, hv := range v.ExtraHeaders {
			hm[hk] = hv
		}
		ex.ExtraHeaders = hm
	}
	return ex
}

func mergeSpecs(out, base, over *Config) {
	// Plugins override wholesale, like SkillsDirs: a non-empty project list
	// replaces the user list entirely (config order is significant — the
	// sync-hook chain runs in this order — so merging entry-by-entry would
	// silently reorder or interleave two unrelated plugin lists).
	pSrc := out.Plugins
	if len(over.Plugins) > 0 {
		pSrc = over.Plugins
	}
	if len(pSrc) > 0 {
		out.Plugins = append([]PluginSpec(nil), pSrc...)
	} else {
		out.Plugins = nil
	}
	if n := len(base.PluginHTTPHeaders) + len(over.PluginHTTPHeaders); n > 0 {
		m := make(map[string]string, n)
		for k, v := range base.PluginHTTPHeaders {
			m[k] = v
		}
		for k, v := range over.PluginHTTPHeaders {
			m[k] = v
		}
		out.PluginHTTPHeaders = m
	} else {
		out.PluginHTTPHeaders = nil
	}
	if n := len(base.MCPServers) + len(over.MCPServers); n > 0 {
		m := make(map[string]MCPServerSpec, n)
		for k, v := range base.MCPServers {
			m[k] = copyMCPServerSpec(v)
		}
		for k, v := range over.MCPServers {
			// A same-name project entry replaces the user entry wholesale
			// (see the MCPServers field doc) rather than merging field by
			// field.
			m[k] = copyMCPServerSpec(v)
		}
		out.MCPServers = m
	} else {
		out.MCPServers = nil
	}
	if n := len(base.Processes) + len(over.Processes); n > 0 {
		m := make(map[string]ProcessSpec, n)
		for k, v := range base.Processes {
			m[k] = copyProcessSpec(v)
		}
		for k, v := range over.Processes {
			// A same-name project entry replaces the user entry wholesale
			// (see the Processes field doc), same as MCPServers.
			m[k] = copyProcessSpec(v)
		}
		out.Processes = m
	} else {
		out.Processes = nil
	}
}
