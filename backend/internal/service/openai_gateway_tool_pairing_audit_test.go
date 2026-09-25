package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsWorkbuddyToolSequenceBrokenError(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"numeric code 11148", `{"code":11148,"msg":"tool calls and tool results do not match"}`, true},
		{"extError string code", `{"code":0,"extError":{"code":"tool_call_sequence_broken"}}`, true},
		{"real 16:52 envelope", `{"code":11148,"msg":"tool calls and tool results do not match, please start a new conversation and retry","requestId":"7e1c3da2","extError":{"code":"tool_call_sequence_broken","message":"tool calls and tool results do not match","type":"invalid_request_error","StatusCode":400}}`, true},
		{"other code 11128", `{"code":11128,"msg":"Illegal API invocation"}`, false},
		{"openai style error", `{"error":{"code":"invalid_request","message":"boom"}}`, false},
		{"empty", ``, false},
		{"not json", `hello`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isWorkbuddyToolSequenceBrokenError([]byte(tt.body)))
		})
	}
}

func TestAuditWorkbuddyToolPairing_ChatClean(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"shell","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_a","content":"ok"},
		{"role":"assistant","content":"done"}
	]}`
	require.Empty(t, auditWorkbuddyToolPairing([]byte(body)))
}

func TestAuditWorkbuddyToolPairing_ChatViolations(t *testing.T) {
	t.Run("missing result", func(t *testing.T) {
		body := `{"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"shell","arguments":"{}"}}]},
			{"role":"user","content":"next"}
		]}`
		problems := auditWorkbuddyToolPairing([]byte(body))
		require.Len(t, problems, 1)
		require.Contains(t, problems[0], `call_a`)
		require.Contains(t, problems[0], "but 0 tool result(s)")
	})

	t.Run("orphan tool result", func(t *testing.T) {
		body := `{"messages":[
			{"role":"user","content":"hi"},
			{"role":"tool","tool_call_id":"call_ghost","content":"ok"}
		]}`
		problems := auditWorkbuddyToolPairing([]byte(body))
		require.Len(t, problems, 1)
		require.Contains(t, problems[0], "orphan result")
	})

	t.Run("tool result without tool_call_id key", func(t *testing.T) {
		body := `{"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"shell","arguments":"{}"}}]},
			{"role":"tool","content":"ok"}
		]}`
		problems := auditWorkbuddyToolPairing([]byte(body))
		require.Len(t, problems, 2) // 缺 id 的孤儿 + call_a 无结果
		require.Contains(t, problems[0], "no tool_call_id key")
	})

	t.Run("non-adjacent result", func(t *testing.T) {
		body := `{"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"shell","arguments":"{}"}}]},
			{"role":"user","content":"interrupt"},
			{"role":"tool","tool_call_id":"call_a","content":"ok"}
		]}`
		problems := auditWorkbuddyToolPairing([]byte(body))
		require.Len(t, problems, 1)
		require.Contains(t, problems[0], "not adjacent")
	})

	t.Run("duplicate call id", func(t *testing.T) {
		body := `{"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"shell","arguments":"{}"}},
				{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_a","content":"ok"}
		]}`
		problems := auditWorkbuddyToolPairing([]byte(body))
		require.Len(t, problems, 2) // 重复 id + 其中一个 call 无结果
		require.Contains(t, problems[0], "duplicate tool_call id")
	})

	t.Run("parallel calls both answered adjacent", func(t *testing.T) {
		body := `{"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"shell","arguments":"{}"}},
				{"id":"call_b","type":"function","function":{"name":"read","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_a","content":"ok"},
			{"role":"tool","tool_call_id":"call_b","content":"ok"}
		]}`
		require.Empty(t, auditWorkbuddyToolPairing([]byte(body)))
	})
}

func TestAuditWorkbuddyToolPairing_ResponsesInput(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		body := `{"input":[
			{"type":"message","role":"user","content":"hi"},
			{"type":"reasoning","summary":[],"id":"msg_d2e308f7"},
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":"ok"}
		]}`
		require.Empty(t, auditWorkbuddyToolPairing([]byte(body)))
	})

	t.Run("missing output", func(t *testing.T) {
		body := `{"input":[
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{}"},
			{"type":"message","role":"user","content":"next"}
		]}`
		problems := auditWorkbuddyToolPairing([]byte(body))
		require.Len(t, problems, 1)
		require.Contains(t, problems[0], "NO matching output item")
	})

	t.Run("orphan output", func(t *testing.T) {
		body := `{"input":[
			{"type":"function_call_output","call_id":"call_ghost","output":"ok"}
		]}`
		problems := auditWorkbuddyToolPairing([]byte(body))
		require.Len(t, problems, 1)
		require.Contains(t, problems[0], "orphan output")
	})
}

func TestAuditWorkbuddyToolPairing_DegradedBodies(t *testing.T) {
	require.Contains(t, auditWorkbuddyToolPairing(nil)[0], "no outbound body")
	require.Contains(t, auditWorkbuddyToolPairing([]byte(`not json`))[0], "not valid JSON")
	require.Contains(t, auditWorkbuddyToolPairing([]byte(`{"model":"x"}`))[0], "neither messages nor input")
}
