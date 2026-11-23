package turn

import (
	"slices"

	"github.com/majorcontext/harness/protocol"
)

// CodeTool is the name of the code tool.
const CodeTool = "code"

// directInCodeMode names the tools that the model calls itself in code mode. Every other tool is a function of a script.
var directInCodeMode = []string{CodeTool, "ask_user_question", "goal", "model", "task", "mcp", "read_tool_result"}

const (
	codeModeText   = "[mode: code. Call tools only through the code tool. The tools that earlier text names, except ask_user_question, goal, model, task, mcp, and read_tool_result, are functions of its script, and a direct call to one of them is refused.]"
	normalModeText = "[mode: normal. Call the tools directly again. The code tool stays available.]"
	noCodeText     = "[mode: normal. Call the tools directly again. The code tool is gone.]"
)

// CodeModeNotice returns the notice that states the mode that begins when the model reads it, for a request that
// follows the first at messages. Normal mode has no text: the tools array states it, so the notice speaks only
// after a code-mode notice.
func CodeModeNotice(code bool, at int) Notice {
	n := Notice{Kind: protocol.PinCodeMode, Cleared: normalModeText, At: at}
	if code {
		n.Text = codeModeText
	}
	return n
}

// NoCodeNotice returns the notice of a request whose declaration lacks the code tool. It speaks only after a
// code-mode notice.
func NoCodeNotice(at int) Notice {
	return Notice{Kind: protocol.PinCodeMode, Cleared: noCodeText, At: at}
}

// refusedInCodeMode returns the error text of a direct call to the tool name in code mode, or "" when the model may
// call it.
func refusedInCodeMode(name string) string {
	if slices.Contains(directInCodeMode, name) {
		return ""
	}
	return "code mode: " + name + " is a function of a script. Call it from the script of a code call, with the same arguments."
}
