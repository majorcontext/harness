package main

type obj map[string]any

func (o obj) set(k string, v any) obj {
	o[k] = v
	return o
}

func (o obj) inMessage(k string, v any) obj {
	o["message"].(obj)[k] = v
	return o
}

func envelope(kind, role string, content []obj) obj {
	return obj{"type": kind, "message": obj{"role": role, "content": content}}
}

func assistant(content ...obj) obj { return envelope("assistant", "assistant", content) }

func user(content ...obj) obj { return envelope("user", "user", content) }

func textBlock(s string) obj { return obj{"type": "text", "text": s} }

func thinkingBlock(s, signature string) obj {
	return obj{"type": "thinking", "thinking": s, "signature": signature}
}

func toolUse(id, name string, input obj) obj {
	return obj{"type": "tool_use", "id": id, "name": name, "input": input}
}

func toolResult(id string, content any, isError bool) obj {
	b := obj{"type": "tool_result", "tool_use_id": id, "content": content}
	if isError {
		b["is_error"] = true
	}
	return b
}

func say(s string) obj { return assistant(textBlock(s)) }

func system(subtype string, fields obj) obj {
	o := obj{"type": "system", "subtype": subtype}
	for k, v := range fields {
		o[k] = v
	}
	return o
}

func tokens(in, out int) obj {
	return obj{"input_tokens": in, "output_tokens": out}
}

func result(subtype string, isError bool, text string, in, out int) obj {
	return obj{
		"type": "result", "subtype": subtype, "is_error": isError,
		"result": text, "usage": tokens(in, out),
	}
}

// failed is the result of a run that failed or was interrupted: the CLI
// reports the failure in errors and sends no result text.
func failed(in, out int, errs ...string) obj {
	return obj{
		"type": "result", "subtype": "error_during_execution", "is_error": true,
		"errors": errs, "usage": tokens(in, out),
	}
}

// interrupted is the diagnostic that the CLI reports for a run that a signal ended.
const interrupted = "[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null"

func success(text string, in, out int) obj { return result("success", false, text, in, out) }

func rateLimitEvent(info obj) obj {
	return obj{"type": "rate_limit_event", "rate_limit_info": info}
}

func userString(s string) obj {
	return obj{"type": "user", "message": obj{"role": "user", "content": s}}
}
