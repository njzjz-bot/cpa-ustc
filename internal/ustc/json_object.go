package ustc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

const jsonObjectInstruction = `{"role":"system","content":"Return exactly one valid JSON object in the final answer. Do not include Markdown code fences, explanations, or text outside the JSON object."}`

func adaptDeepSeekJSONObject(base, model string, body map[string]json.RawMessage) bool {
	if model != "deepseek-flash" || !schoolURL(base) {
		return false
	}
	var format struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(body["response_format"], &format) != nil || format.Type != "json_object" {
		return false
	}
	if raw, present := body["thinking"]; present && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var thinking struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &thinking) != nil || thinking.Type != "enabled" {
			return false
		}
	}
	var messages []json.RawMessage
	if json.Unmarshal(body["messages"], &messages) != nil || len(messages) == 0 {
		return false
	}
	body["messages"], _ = json.Marshal(append([]json.RawMessage{json.RawMessage(jsonObjectInstruction)}, messages...))
	delete(body, "response_format")
	return true
}

type completionText struct {
	Content      string            `json:"content"`
	Refusal      string            `json:"refusal"`
	ToolCalls    []json.RawMessage `json:"tool_calls"`
	FunctionCall *json.RawMessage  `json:"function_call"`
}

func (text completionText) hasNonTextResult() bool {
	return text.Refusal != "" || len(text.ToolCalls) > 0 || text.FunctionCall != nil
}

func requiresJSONObject(finish string) bool {
	switch finish {
	case "length", "content_filter", "tool_calls", "function_call":
		return false
	default:
		return true
	}
}

func validJSONObject(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), "{") && json.Valid([]byte(content))
}

func invalidJSONObject() *StatusError {
	return &StatusError{Status: http.StatusBadGateway, Message: "学校模型未返回完整的 JSON 对象正文"}
}

// ValidateContent checks only those JSON-object requests adapted by the plugin.
func (response *CompletionResponse) ValidateContent(body []byte) error {
	if !response.jsonObject {
		return nil
	}
	var result struct {
		Choices []struct {
			FinishReason string         `json:"finish_reason"`
			Message      completionText `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &result) != nil || len(result.Choices) == 0 {
		return invalidJSONObject()
	}
	for _, choice := range result.Choices {
		if requiresJSONObject(choice.FinishReason) && !choice.Message.hasNonTextResult() && !validJSONObject(choice.Message.Content) {
			return invalidJSONObject()
		}
	}
	return nil
}

type jsonObjectChoice struct {
	text     strings.Builder
	nonText  bool
	finished bool
}

// Relay preserves incremental SSE and rejects invalid JSON before reporting a
// successful completion. Already forwarded partial output is not replayed.
func (response *CompletionResponse) Relay(emit func([]byte) error) error {
	if !response.jsonObject {
		return RelaySSE(response.Body, response.Header, emit)
	}
	const maxJSONBytes = 64 << 20
	var total int
	choices := map[int]*jsonObjectChoice{}
	complete := func() bool {
		for _, choice := range choices {
			if !choice.finished {
				return false
			}
		}
		return len(choices) > 0
	}
	err := RelaySSE(response.Body, response.Header, func(event []byte) error {
		data, _ := ssePayload(event)
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			return emit(event)
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			if !complete() {
				return invalidJSONObject()
			}
			return emit(event)
		}
		var chunk struct {
			Choices []struct {
				Index        int            `json:"index"`
				FinishReason string         `json:"finish_reason"`
				Delta        completionText `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return invalidJSONObject()
		}
		for _, choice := range chunk.Choices {
			state := choices[choice.Index]
			if state == nil {
				state = &jsonObjectChoice{}
				choices[choice.Index] = state
			}
			if state.finished {
				return invalidJSONObject()
			}
			total += len(choice.Delta.Content)
			if total > maxJSONBytes {
				return &StatusError{Status: http.StatusBadGateway, Message: "学校模型 JSON 正文累计超过 64 MiB"}
			}
			state.text.WriteString(choice.Delta.Content)
			state.nonText = state.nonText || choice.Delta.hasNonTextResult()
			if choice.FinishReason != "" {
				if requiresJSONObject(choice.FinishReason) && !state.nonText && !validJSONObject(state.text.String()) {
					return invalidJSONObject()
				}
				state.finished = true
			}
		}
		return emit(event)
	})
	if err != nil {
		return err
	}
	if !complete() {
		return invalidJSONObject()
	}
	return nil
}
