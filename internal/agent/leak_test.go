package agent

import "testing"

func TestLeakedToolCall(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        string
		wantProse string
		wantTool  string
		wantLeak  bool
	}{
		{
			name:      "hermes xml after prose",
			in:        "Preparo el email.<tool_call>\n<function=exec>\n<parameter=command>ls</parameter>\n</function>\n</tool_call>",
			wantProse: "Preparo el email.",
			wantTool:  "exec",
			wantLeak:  true,
		},
		{
			name:     "hermes json, markup only",
			in:       "<tool_call>\n{\"name\": \"send_email\", \"arguments\": {\"to\": \"x@y.z\"}}\n</tool_call>",
			wantTool: "send_email",
			wantLeak: true,
		},
		{
			name:      "mistral block",
			in:        "Listo. [TOOL_CALLS] [{\"name\": \"web_search\", \"arguments\": {}}]",
			wantProse: "Listo.",
			wantTool:  "web_search",
			wantLeak:  true,
		},
		{
			name:     "bare llama function tag",
			in:       "<function=browser_read>{\"filter\": \"vino\"}</function>",
			wantTool: "browser_read",
			wantLeak: true,
		},
		{
			name:     "deepseek sentinel, no name found",
			in:       "<|tool▁calls▁begin|>…garbled…",
			wantLeak: true,
		},
		{
			name: "plain prose untouched",
			in:   "Encontré seis vinos con envío gratis; el mejor es el Portillo Malbec.",
		},
		{
			name: "html in prose untouched",
			in:   "Use a <b>bold</b> tag, or call the function like `f(x)`.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prose, tool, leaked := leakedToolCall(tc.in)
			if leaked != tc.wantLeak {
				t.Fatalf("leaked = %v, want %v", leaked, tc.wantLeak)
			}
			if prose != tc.wantProse {
				t.Errorf("prose = %q, want %q", prose, tc.wantProse)
			}
			if tool != tc.wantTool {
				t.Errorf("tool = %q, want %q", tool, tc.wantTool)
			}
		})
	}
}
