package turn

import "github.com/majorcontext/harness/protocol"

func replayStopContext(req Request) StopContext {
	sc := StopContext{SessionID: req.SessionID, TurnID: req.TurnID}
	calls := make(map[string]protocol.ToolCall)
	for _, item := range req.ResumeItems {
		for _, p := range item.Parts {
			switch {
			case item.Role == protocol.RoleAssistant && p.Type == protocol.LogPartText:
				sc.TurnText += p.Text
			case item.Role == protocol.RoleAssistant && p.Type == protocol.LogPartToolCall:
				calls[p.CallID] = protocol.ToolCall{ID: p.CallID, Name: p.Name, Arguments: p.Arguments}
			case item.Role == protocol.RoleTool && p.Type == protocol.LogPartToolResult:
				if c, ok := calls[p.CallID]; ok {
					sc.ToolCalls = append(sc.ToolCalls, cloneStopToolCall(StopToolCall{Call: c, Result: protocol.ToolResult{Text: p.Text, IsError: p.IsError}}))
					delete(calls, p.CallID)
				}
			}
		}
	}
	return sc
}
