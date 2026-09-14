package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// The model is asked to open with a line before its first tool call and
// routinely does not, which leaves the user watching a turn that has said
// nothing. The filler writes that line instead, on a chain fast enough for it
// to still be worth hearing.
func TestFillerSpeaksWhileATurnRunsLong(t *testing.T) {
	h := newHarness(t, toolCall("probe", map[string]any{"value": "abc"}), final("done"))
	h.tool.block = make(chan struct{})
	light := &scriptedChat{script: []func(*provider.Request) (*provider.Response, error){
		final("Estoy mirando eso."),
	}}
	h.loop.WithLight(light)
	h.loop.fillGrace, h.loop.fillEvery = 20*time.Millisecond, time.Hour

	lines := make(chan string, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.loop.ProcessDirectNotice(context.Background(), "buscá la cosa", "cli:test", "", "",
			func(line string) { lines <- line })
	}()

	select {
	case got := <-lines:
		if got != "Estoy mirando eso." {
			t.Errorf("filler said %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the turn ran in silence")
	}
	close(h.tool.block)
	<-done

	// It is told what the user asked, so the line can say something about it.
	light.mu.Lock()
	req := light.requests[0]
	light.mu.Unlock()
	if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "buscá la cosa") {
		t.Errorf("the filler was not told what the user said: %q", req.Messages[len(req.Messages)-1].Content)
	}
}

// Nothing the filler says belongs in the conversation: it is the channel
// keeping the user company, and a transcript holding it would offer it back
// the next time somebody asks what was said.
func TestFillerIsNotPartOfTheConversation(t *testing.T) {
	h := newHarness(t, toolCall("probe", map[string]any{"value": "abc"}), final("done"))
	h.tool.block = make(chan struct{})
	h.loop.WithLight(&scriptedChat{script: []func(*provider.Request) (*provider.Response, error){
		final("Estoy mirando eso."),
	}})
	h.loop.fillGrace, h.loop.fillEvery = 20*time.Millisecond, time.Hour

	lines := make(chan string, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.loop.ProcessDirectNotice(context.Background(), "buscá la cosa", "cli:test", "", "",
			func(line string) { lines <- line })
	}()
	<-lines
	close(h.tool.block)
	<-done

	history, _ := h.store.History("cli:test")
	for _, m := range history {
		if strings.Contains(m.Content, "Estoy mirando eso.") {
			t.Fatalf("the filler was persisted: %+v", m)
		}
	}
}

// With no chain to write the line there is no line. Falling back to the
// conversation's own chain would ask the model the user is already waiting
// on, at the prompt size and reasoning effort that made them wait.
func TestFillerIsSilentWithoutALightChain(t *testing.T) {
	h := newHarness(t, final("done"))
	h.loop.fillGrace, h.loop.fillEvery = 20*time.Millisecond, time.Hour
	if h.loop.lightChat() != nil {
		t.Fatal("a harness with no light chain should have none")
	}
	said := 0
	reply, err := h.loop.ProcessDirectNotice(context.Background(), "hola", "cli:test", "", "",
		func(string) { said++ })
	if err != nil {
		t.Fatal(err)
	}
	if reply != "done" || said != 0 {
		t.Errorf("reply = %q, notices = %d", reply, said)
	}
}

// What the turn has been doing is the whole substance of the line. The
// arguments are not: a second model has no business with the user's paths and
// searches, and the shape of the work is all the sentence needs.
func TestFillerStateCarriesToolNamesAndNotArguments(t *testing.T) {
	state := fillerState("open the report", []string{"read_file", "exec"}, nil)
	if !strings.Contains(state, "read_file, exec") {
		t.Errorf("tool names missing: %q", state)
	}
	if strings.Contains(state, "/home/") {
		t.Errorf("state should carry no paths: %q", state)
	}
	// A long turn is summarized rather than recited.
	many := make([]string, 0, fillerToolsShown+3)
	for range fillerToolsShown + 3 {
		many = append(many, "read_file")
	}
	many[0] = "first_call"
	if got := fillerState("x", many, nil); strings.Contains(got, "first_call") {
		t.Errorf("the oldest calls should fall off: %q", got)
	}
	// It repeats neither itself nor the assistant.
	if got := fillerState("x", nil, []string{"Dame un momento."}); !strings.Contains(got, "Do not say it again") {
		t.Errorf("said lines missing: %q", got)
	}
}

// A model told to answer with one line occasionally answers with two.
func TestFirstLineTrimsToOneSentence(t *testing.T) {
	if got := firstLine("  Estoy mirando eso.\nY después te cuento.  "); got != "Estoy mirando eso." {
		t.Errorf("firstLine = %q", got)
	}
}

// The language the reply is spoken in is not negotiable: a history-less line
// read out by a Spanish voice in English is noise.
func TestFillerRulesNameTheSpokenLanguage(t *testing.T) {
	if !strings.Contains(fillerRules("es"), `"es"`) {
		t.Error("the language code should reach the filler's brief")
	}
	if !strings.Contains(fillerRules(""), "same language the user used") {
		t.Error("with no code set, the filler follows the user")
	}
}

// A heartbeat, a cron job and a delegated job run with nobody holding the
// line, and their notices are published to whichever chat the user last used.
// A filler there is not company; it is the machine interrupting an empty room
// to say it is busy.
func TestFillerStaysOutOfTurnsNobodyIsWaitingOn(t *testing.T) {
	h := newHarness(t)
	light := &scriptedChat{}
	h.loop.WithLight(light)
	h.loop.fillGrace, h.loop.fillEvery = 10*time.Millisecond, 10*time.Millisecond

	for _, in := range []turnInput{
		{trigger: "cron", sessionKey: "cron:nightly"},
		{trigger: "job", sessionKey: "job:j1"},
		{trigger: "heartbeat", sessionKey: "system:heartbeat", ephemeral: true},
	} {
		f := h.loop.fill(context.Background(), in)
		t.Cleanup(f.stop)
	}
	time.Sleep(200 * time.Millisecond) // many graces

	light.mu.Lock()
	defer light.mu.Unlock()
	if len(light.requests) != 0 {
		t.Errorf("%d filler calls for turns nobody was waiting on", len(light.requests))
	}
}

// ask_user is the one tool whose runtime is the user thinking. The silence is
// theirs to break, and filling it is talking over an answer.
func TestFillerIsQuietWhileTheUserIsBeingAsked(t *testing.T) {
	h := newHarness(t)
	light := &scriptedChat{script: []func(*provider.Request) (*provider.Response, error){
		final("Sigo en eso."),
	}}
	h.loop.WithLight(light)

	f := &filler{loop: h.loop, in: turnInput{sessionKey: "cli:test", content: "hola"}}
	f.running("read_file", tools.AskToolName)
	if got := f.compose(context.Background()); got != "" {
		t.Errorf("the filler talked over a question: %q", got)
	}
	// The answer is in; the turn is Factor's again and so is the silence.
	f.finished()
	if got := f.compose(context.Background()); got != "Sigo en eso." {
		t.Errorf("filler = %q after the question was answered", got)
	}
}

// Composing takes a call, and the answer can land while it is in flight. A
// line arriving behind the thing it said it was about to do is worse than
// silence, so the turn outrunning its filler drops it.
func TestFillerDropsALineTheAnswerHasOutrun(t *testing.T) {
	h := newHarness(t)
	f := &filler{loop: h.loop, in: turnInput{sessionKey: "cli:test", content: "hola"},
		said: make(chan struct{}, 1), done: make(chan struct{})}
	said := make(chan string, 1)
	f.in.notice = func(line string) { said <- line }

	// The turn answers while the filler's call is still out.
	h.loop.WithLight(&scriptedChat{script: []func(*provider.Request) (*provider.Response, error){
		func(*provider.Request) (*provider.Response, error) {
			f.stop()
			return &provider.Response{Content: "Estoy mirando eso."}, nil
		},
	}})
	f.speak(context.Background())

	select {
	case line := <-said:
		t.Errorf("a filler landed behind the answer: %q", line)
	default:
	}
}
