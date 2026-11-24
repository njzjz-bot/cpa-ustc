package ustc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDeepSeekJSONModeChangesOnlyAffectedRequests(t *testing.T) {
	for _, test := range []struct {
		name, base, model, options string
		adapt                      bool
	}{
		{"default thinking", APIBase, "deepseek-flash", `"response_format":{"type":"json_object"}`, true},
		{"explicit thinking", APIBase, "deepseek-flash", `"response_format":{"type":"json_object"},"thinking":{"type":"enabled"}`, true},
		{"thinking off", APIBase, "deepseek-flash", `"response_format":{"type":"json_object"},"thinking":{"type":"disabled"}`, false},
		{"schema", APIBase, "deepseek-flash", `"response_format":{"type":"json_schema","json_schema":{"strict":true}}`, false},
		{"text", APIBase, "deepseek-flash", `"thinking":{"type":"enabled"}`, false},
		{"ascend", APIBase, "deepseek-v4-flash-ascend", `"response_format":{"type":"json_object"}`, false},
		{"custom upstream", "https://example.test/v1", "deepseek-flash", `"response_format":{"type":"json_object"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultConfig()
			config.Network = "direct"
			config.APIBase = test.base
			service := NewService(config)
			t.Cleanup(service.Close)
			account, err := APIAccount("synthetic-key", "", config)
			if err != nil {
				t.Fatal(err)
			}
			const originalMessages = `[{"role":"system","content":"existing instruction"},{"role":"user","content":[{"type":"text","text":"request"}]}]`
			payload := []byte(`{"model":"input-alias","max_tokens":100,"tool_choice":"auto","messages":` + originalMessages + `,` + test.options + `}`)
			var original map[string]json.RawMessage
			if err := json.Unmarshal(payload, &original); err != nil {
				t.Fatal(err)
			}
			service.apiClient = func(string) *http.Client {
				return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					var wire map[string]json.RawMessage
					if err := json.NewDecoder(request.Body).Decode(&wire); err != nil {
						t.Fatal(err)
					}
					if got, want := string(wire["model"]), `"`+test.model+`"`; got != want {
						t.Errorf("expected resolved model %s, got %s", want, got)
					}
					for _, key := range []string{"thinking", "max_tokens", "tool_choice"} {
						if string(wire[key]) != string(original[key]) {
							t.Errorf("%s was modified: %s", key, wire[key])
						}
					}
					if test.adapt {
						want := "[" + jsonObjectInstruction + "," + originalMessages[1:]
						if len(wire["response_format"]) != 0 || string(wire["messages"]) != want {
							t.Errorf("JSON compatibility changed the wrong fields: messages=%s, response_format=%s", wire["messages"], wire["response_format"])
						}
					} else if string(wire["messages"]) != originalMessages || string(wire["response_format"]) != string(original["response_format"]) {
						t.Error("unaffected request was rewritten")
					}
					return jsonResponse(map[string]any{}), nil
				})}
			}
			result, _, err := service.Execute(context.Background(), account, test.model, payload, false)
			if err != nil {
				t.Fatal(err)
			}
			result.Body.Close()
			if result.jsonObject != test.adapt {
				t.Fatalf("wrong JSON validator selection: %t", result.jsonObject)
			}
		})
	}
}

func TestDeepSeekJSONObjectValidation(t *testing.T) {
	response := &CompletionResponse{jsonObject: true}
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"JSON object", `{"choices":[{"finish_reason":"stop","message":{"content":"{\"ok\":true}","reasoning_content":"unrelated"}}]}`, true},
		{"reasoning only", `{"choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_content":"{\"ok\":true}"}}]}`, false},
		{"JSON array", `{"choices":[{"finish_reason":"stop","message":{"content":"[1,2]"}}]}`, false},
		{"prefix with trailing text", `{"choices":[{"finish_reason":"stop","message":{"content":"{} extra"}}]}`, false},
		{"truncated", `{"choices":[{"finish_reason":"length","message":{"content":"{\"ok\":"}}]}`, true},
		{"tool call", `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"tool-0"}]}}]}`, true},
		{"refusal", `{"choices":[{"finish_reason":"stop","message":{"refusal":"declined"}}]}`, true},
		{"empty choices", `{"choices":[]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := response.ValidateContent([]byte(test.body))
			if (err == nil) != test.valid {
				t.Fatalf("validation = %v, want valid=%t", err, test.valid)
			}
			if err != nil {
				var status *StatusError
				if !errors.As(err, &status) || status.Status != http.StatusBadGateway {
					t.Fatalf("validation was not reported as an upstream error: %v", err)
				}
			}
		})
	}
}

func TestDeepSeekJSONObjectStreamValidation(t *testing.T) {
	const reasoning = "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"{\\\"ok\\\":true}\"},\"finish_reason\":null}]}\n\n"
	const first = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{\\\"ok\\\":\"}}]}\n\n"
	const second = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"true}\"}}]}\n\n"
	const stop = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	const length = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"
	const tool = "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"t\"}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	const done = "data: [DONE]\n\n"
	for _, test := range []struct {
		name, stream, forwarded string
		valid                   bool
	}{
		{"complete", reasoning + first + second + stop + done, reasoning + first + second + stop + done, true},
		{"reasoning only", reasoning + stop + done, reasoning, false},
		{"invalid at stop", first + stop + done, first, false},
		{"incomplete at done", first + second + done, first + second, false},
		{"incomplete at EOF", first, first, false},
		{"length", first + length + done, first + length + done, true},
		{"tool call", tool + done, tool + done, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &CompletionResponse{
				Response:   &http.Response{Body: io.NopCloser(strings.NewReader(test.stream))},
				jsonObject: true,
			}
			var output strings.Builder
			err := response.Relay(func(event []byte) error {
				output.Write(event)
				return nil
			})
			if (err == nil) != test.valid || output.String() != test.forwarded {
				t.Fatalf("relay = %v, output = %q", err, output.String())
			}
		})
	}
}
