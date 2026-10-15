package main

import (
	"os"
	"slices"
	"strings"
)

// searchModes cover the built-in WebSearch tool of the CLI. "web_search" runs
// one search and answers from it; the init frame lists the tools of --tools,
// as the CLI does.
var searchModes = map[string]mode{
	"web_search":       webSearchTurn,
	"web_search_plain": webSearchPlain,
	"web_search_refused": func(f *fake) {
		msg := "Error: No such tool available: WebSearch. WebSearch is disabled for this session, in subagents as well as here."
		f.emit(
			assistant(toolUse("toolu_ws", "WebSearch", obj{"query": "current Go release", "mode": "standard"})),
			user(toolResult("toolu_ws", "<tool_use_error>"+msg+"</tool_use_error>", true)).set("tool_use_result", msg),
			say("Done."),
			success("Done.", 9, 4),
		)
	},
}

const plainResult = "Web search results for query: \"current Go release\"\n\nGo 1.27.2 is the current release."

// webSearchPlain answers a search with a result text that holds no list of sources and no structured result.
func webSearchPlain(f *fake) {
	f.emit(
		assistant(toolUse("toolu_ws", "WebSearch", obj{"query": "current Go release", "mode": "standard"})),
		user(toolResult("toolu_ws", plainResult, false)),
		say("Go 1.27.2 is the current release."),
		success("Go 1.27.2 is the current release.", 9, 4),
	)
}

const searchResult = "Web search results for query: \"current Go release\"\n\nLinks: [{\"title\":\"Release History\",\"url\":\"https://go.dev/doc/devel/release\"},{\"title\":\"Go 1.26 Release Notes\",\"url\":\"https://go.dev/doc/go1.26\"}]\n\nGo 1.27.2 is the current release."

func webSearchTurn(f *fake) {
	f.emit(
		assistant(toolUse("toolu_ws", "WebSearch", obj{"query": "current Go release", "mode": "standard"})),
		user(toolResult("toolu_ws", searchResult, false)).set("tool_use_result", obj{"query": "current Go release", "results": []any{obj{"content": []obj{
			{"title": "Release History", "url": "https://go.dev/doc/devel/release"},
			{"title": "Go 1.26 Release Notes", "url": "https://go.dev/doc/go1.26"}}}, "Go 1.27.2 is the current release."}}),
		say("Go 1.27.2 is the current release (https://go.dev/doc/devel/release)."),
		success("Go 1.27.2 is the current release (https://go.dev/doc/devel/release).", 9, 4),
	)
}

// argTools lists the tools of the --tools flag of this process.
func argTools() []string {
	i := slices.Index(os.Args, "--tools")
	if i < 0 || i+1 >= len(os.Args) || os.Args[i+1] == "" {
		return []string{}
	}
	return strings.Split(os.Args[i+1], ",")
}
