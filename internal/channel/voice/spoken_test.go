package voice

import (
	"strings"
	"testing"
	"time"
)

func TestSpokenTextRewritesMarkdownIntoProse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain prose is untouched",
			"It is 25 degrees outside. Nice day for a walk.",
			"It is 25 degrees outside. Nice day for a walk."},
		{"emphasis",
			"That is **very** important, *really* — even ~~quite~~ vital.",
			"That is very important, really — even quite vital."},
		{"headers and bullets",
			"## Plan\n- buy milk\n* call Ana\n+ rest",
			"Plan\nbuy milk\ncall Ana\nrest"},
		{"links speak their text",
			"See [the release notes](https://github.com/cyqlelabs/factor/releases) for more.",
			"See the release notes for more."},
		{"bare urls speak their host",
			"It is on https://www.github.com/cyqlelabs/factor now.",
			"It is on github.com now."},
		{"inline code drops its ticks",
			"Run `factor status` to check.",
			"Run factor status to check."},
		{"underscores read as spaces",
			"The field is channels.voice.wake_word, mind the case.",
			"The field is channels.voice.wake word, mind the case."},
		{"blockquotes and rules",
			"> as they say\n---\nmoving on",
			"as they say\nmoving on"},
		{"tables become lists",
			"| tier | cost |\n|------|------|\n| cloud | low |",
			"tier, cost\ncloud, low"},
		{"html entities",
			"Ben &amp; Jerry &gt; the rest",
			"Ben & Jerry > the rest"},
	}
	for _, tc := range cases {
		if got := spokenText(tc.in, "en"); got != tc.want {
			t.Errorf("%s:\n in  %q\n got %q\n want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Nobody wants code read out token by token: blocks become a short localized
// note, closed or not.
func TestSpokenTextOmitsCodeBlocks(t *testing.T) {
	in := "Here you go:\n```go\nfunc main() {}\n```\nDone."
	got := spokenText(in, "en")
	if strings.Contains(got, "func main") || !strings.Contains(got, "(code omitted)") {
		t.Errorf("got %q", got)
	}
	if es := spokenText(in, "es-MX"); !strings.Contains(es, "(código omitido)") {
		t.Errorf("es: %q", es)
	}
	dangling := spokenText("Sure:\n```python\nprint('hi')", "en")
	if strings.Contains(dangling, "print") || !strings.Contains(dangling, "(code omitted)") {
		t.Errorf("unclosed fence: %q", dangling)
	}
}

// The seatbelt sits on the speech path itself: a markdown reply reaches the
// synthesiser as prose, whatever the model was asked for.
func TestVoiceSpeaksProseNotMarkdown(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.mu.Lock()
	h.reply = "**Done!** See [the log](https://example.com/log) or run `make check`."
	h.mu.Unlock()
	h.start()
	h.say()
	h.turn(10 * time.Second)

	waitUntil(t, func() bool { return len(h.synthesized()) > 0 })
	spoken := h.synthesized()[len(h.synthesized())-1]
	if strings.ContainsAny(spoken, "*`[]") {
		t.Errorf("markdown reached the synthesiser: %q", spoken)
	}
	if !strings.Contains(spoken, "Done! See the log or run make check.") {
		t.Errorf("the prose did not survive: %q", spoken)
	}
}
