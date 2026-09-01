package agent

import (
	"regexp"
	"strings"
)

// A model asked for its final answer with no tools sometimes writes the tool
// call anyway, in its own chat template's text form. With no structured tool
// channel in the request, that markup arrives as assistant content, and
// shipped verbatim it is the worst reply available: the user reads a wall of
// syntax, and the prose around it ("I'll send the email now") claims work
// that never ran. leakMarkers are the openings of the template formats seen
// in the wild — Hermes/Qwen-style XML, Mistral's block, a bare Llama-style
// function tag, DeepSeek's and Kimi's sentinels — each specific enough that
// ordinary prose will not contain it.
var leakMarkers = []string{
	"<tool_call>",
	"<|tool_call|>",
	"[TOOL_CALLS]",
	"<function=",
	"<|tool▁calls▁begin|>",
	"<|tool_calls_section_begin|>",
}

// leakToolName pulls the tool's name out of the markup, whichever of the two
// shapes carries it: a <function=name> tag or a JSON "name" field.
var leakToolName = []*regexp.Regexp{
	regexp.MustCompile(`<function=([A-Za-z0-9_.-]+)`),
	regexp.MustCompile(`"name"\s*:\s*"([A-Za-z0-9_.-]+)"`),
}

// leakedToolCall reports whether an answer holds a tool call written as
// text. It returns the prose ahead of the markup (often empty — the call may
// be the whole answer) and the tool the model was trying to run, when the
// markup names one.
func leakedToolCall(content string) (prose, tool string, leaked bool) {
	at := -1
	for _, marker := range leakMarkers {
		if i := strings.Index(content, marker); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 {
		return "", "", false
	}
	for _, re := range leakToolName {
		if m := re.FindStringSubmatch(content[at:]); m != nil {
			tool = m[1]
			break
		}
	}
	return strings.TrimSpace(content[:at]), tool, true
}
