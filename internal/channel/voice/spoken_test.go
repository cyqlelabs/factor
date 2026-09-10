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

// A reply is cut for synthesis so the first request is short and every later
// one may be twice its predecessor: the speakers start on one sentence while
// the rest renders behind it.
func TestSpeechChunksStartShortAndGrow(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"one sentence is one request",
			"It is sunny in Madrid today.",
			[]string{"It is sunny in Madrid today."}},
		{"the first sentence goes alone",
			"Yes, I can do that for you. The forecast says rain from four. Take an umbrella.",
			[]string{"Yes, I can do that for you.", "The forecast says rain from four. Take an umbrella."}},
		{"a one-word opener rides with the next sentence",
			"Sí. Mañana llueve por la tarde, así que llevá paraguas. Nada más por hoy.",
			[]string{"Sí. Mañana llueve por la tarde, así que llevá paraguas.", "Nada más por hoy."}},
		{"a line break ends a sentence",
			"First point without a full stop\nSecond point without one either",
			[]string{"First point without a full stop", "Second point without one either"}},
		{"a decimal is not a sentence end",
			"It costs 3.50 at the corner shop today. Cheaper than yesterday, which was 4.25 at noon.",
			[]string{"It costs 3.50 at the corner shop today.", "Cheaper than yesterday, which was 4.25 at noon."}},
		{"a closing quote stays with its sentence",
			`She looked up and said "no way!" Then she left the room quietly.`,
			[]string{`She looked up and said "no way!"`, "Then she left the room quietly."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := speechChunks(tc.in, ttsChunkChars)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("speechChunks(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Each request may be twice the one before it and never past the limit; a
// sentence is only ever split when it alone is longer than the limit.
func TestSpeechChunksGrowGeometricallyUpToTheLimit(t *testing.T) {
	sentence := "This sentence is exactly forty characters"
	sentence += strings.Repeat(" more", 2) + "." // 51 bytes
	text := strings.Repeat(sentence+" ", 40)
	chunks := speechChunks(text, 300)
	if len(chunks) < 4 {
		t.Fatalf("got %d chunks, want the reply spread over several", len(chunks))
	}
	if got := len(chunks[0]); got != len(sentence) {
		t.Errorf("first chunk is %d bytes, want the first sentence (%d)", got, len(sentence))
	}
	for i := 1; i < len(chunks); i++ {
		if len(chunks[i]) > 300 {
			t.Errorf("chunk %d is %d bytes, past the limit", i, len(chunks[i]))
		}
		if len(chunks[i]) > chunkGrowth*len(chunks[i-1]) {
			t.Errorf("chunk %d (%d bytes) more than doubled chunk %d (%d bytes)", i, len(chunks[i]), i-1, len(chunks[i-1]))
		}
		if !strings.HasSuffix(chunks[i], ".") {
			t.Errorf("chunk %d does not end on a sentence: %q", i, chunks[i])
		}
	}
	if strings.Join(chunks, " ") != strings.TrimSpace(text) {
		t.Error("the chunks do not add back up to the reply")
	}

	runOn := strings.Repeat("word ", 100) // no sentence end at all
	for i, chunk := range speechChunks(runOn, 120) {
		if len(chunk) > 120 {
			t.Errorf("run-on chunk %d is %d bytes, past the limit", i, len(chunk))
		}
	}
}
