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
	state := fillerState("open the report", []string{"read_file", "exec"}, nil, "")
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
	if got := fillerState("x", many, nil, ""); strings.Contains(got, "first_call") {
		t.Errorf("the oldest calls should fall off: %q", got)
	}
	// It repeats neither itself nor the assistant.
	if got := fillerState("x", nil, []string{"Dame un momento."}, ""); !strings.Contains(got, "Dame un momento.") {
		t.Errorf("said lines missing: %q", got)
	}
}

// Recall is strongest at the two ends of a prompt, and the request is the one
// thing here that pulls toward answering it. Ending on the request put the
// temptation in the best seat in the house and the rule against it in the
// worst, so the state block closes on the instruction instead.
func TestFillerStateEndsOnTheInstructionRatherThanTheRequest(t *testing.T) {
	state := fillerState("¿cuánto gasté este mes?", []string{"read_file"}, nil, "es")
	tail := state[len(state)/2:]
	if !strings.Contains(tail, "not the answer") {
		t.Errorf("the closing constraint is missing: %q", tail)
	}
	if strings.HasSuffix(strings.TrimSpace(state), "¿cuánto gasté este mes?") {
		t.Error("the prompt ends on the request it must not answer")
	}
	if !strings.Contains(tail, `"es"`) {
		t.Errorf("the language is not restated where it is read: %q", tail)
	}
}

// A request runs as long as the user felt like talking; the filler needs its
// subject, not all of it.
func TestFillerStateClipsALongRequest(t *testing.T) {
	long := strings.Repeat("palabra ", 200)
	if got := fillerState(long, nil, nil, ""); len(got) > fillerRequestChars+600 {
		t.Errorf("state grew to %d bytes on a long request", len(got))
	}
}

// A model this size follows a worked example further than a paragraph about
// one, and an example in the wrong language pulls the answer with it.
func TestFillerExamplesAreWrittenInTheLineSLanguage(t *testing.T) {
	if !strings.Contains(fillerExamples("es"), "Estoy mirando el pronóstico.") {
		t.Error("a Spanish line needs Spanish demonstrations")
	}
	if !strings.Contains(fillerExamples("en"), "I'm checking the forecast.") {
		t.Error("English demonstrations missing")
	}
	// Each one pairs a request with a line that conspicuously does not answer
	// it, which is the whole thing being demonstrated.
	for _, lang := range []string{"es", "en"} {
		if strings.Count(fillerExamples(lang), "Request:") < 3 {
			t.Errorf("%s: too few demonstrations to set a pattern", lang)
		}
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
	if !strings.Contains(fillerRules(""), "same language as the request") {
		t.Error("with no code set, the filler follows the request")
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

// A channel that names no language leaves it to the request, and the request
// a person sends mid-task is "y?". Read alone it is nothing, the English
// examples decide, and a Spanish conversation gets fifteen minutes of English
// lines. The conversation so far is what settles it.
func TestFillerLearnsTheConversationsLanguage(t *testing.T) {
	spanish := []provider.Message{
		{Role: "user", Content: "buscame departamentos en Concordia"},
		{Role: "assistant", Content: "Listo, arranco con Argenprop."},
		{Role: "user", Content: "detene el navegador y reinicia la búsqueda"},
		{Role: "user", Content: "[system] Background job j1 finished with state done. Report the outcome to the user concisely."},
	}
	if got := conversationLanguage("y?", spanish); got != "es" {
		t.Errorf("language = %q for a Spanish conversation asked \"y?\"", got)
	}
	english := []provider.Message{
		{Role: "user", Content: "find me the flights for Friday and check the prices"},
		{Role: "user", Content: "what did you find?"},
	}
	if got := conversationLanguage("and?", english); got != "" {
		t.Errorf("language = %q for an English conversation", got)
	}
	if got := conversationLanguage("ok", nil); got != "" {
		t.Errorf("language = %q with nothing to read", got)
	}

	// The turn hands the history over once it is loaded; a channel that
	// names a language keeps it.
	h := newHarness(t)
	f := h.loop.fill(context.Background(), turnInput{sessionKey: "telegram:1", content: "y?", trigger: "user"})
	t.Cleanup(f.stop)
	f.learn(spanish)
	if f.language != "es" {
		t.Errorf("learned %q", f.language)
	}
	spoken := h.loop.fill(context.Background(), turnInput{sessionKey: "voice:local", content: "y?", trigger: "user",
		toolCtx: tools.ToolContext{Language: "en"}})
	t.Cleanup(spoken.stop)
	spoken.learn(spanish)
	if spoken.language != "en" {
		t.Errorf("a channel's language was overridden: %q", spoken.language)
	}
}

// The language it learned is the language the brief is written in — the
// rule and, above all, the examples, since an example in the wrong language
// pulls the line with it.
func TestFillerBriefFollowsTheLearnedLanguage(t *testing.T) {
	h := newHarness(t, toolCall("probe", map[string]any{"value": "abc"}), final("listo"))
	h.tool.block = make(chan struct{})
	light := &scriptedChat{script: []func(*provider.Request) (*provider.Response, error){
		final("Estoy mirando eso."),
	}}
	h.loop.WithLight(light)
	h.loop.fillGrace, h.loop.fillEvery = 20*time.Millisecond, time.Hour
	for _, m := range []provider.Message{
		{Role: "user", Content: "buscame departamentos en Concordia"},
		{Role: "assistant", Content: "Listo, arranco."},
	} {
		if err := h.store.Append("cli:test", m); err != nil {
			t.Fatal(err)
		}
	}

	lines := make(chan string, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.loop.ProcessDirectNotice(context.Background(), "y?", "cli:test", "", "",
			func(line string) { lines <- line })
	}()
	select {
	case <-lines:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn ran in silence")
	}
	close(h.tool.block)
	<-done

	light.mu.Lock()
	req := light.requests[0]
	light.mu.Unlock()
	if !strings.Contains(req.Messages[0].Content, "Estoy mirando el pronóstico.") {
		t.Errorf("the examples are not in the conversation's language:\n%s", req.Messages[0].Content)
	}
	if !strings.Contains(req.Messages[1].Content, `code "es"`) {
		t.Errorf("the instruction does not name the language:\n%s", req.Messages[1].Content)
	}
}

// A written chat is not silent while a turn works — the typing indicator is
// on the whole time — and every line there is a message that stays in the
// thread. So it waits longer before the first one and longer between them
// than a voice in a room does.
func TestFillerWaitsLongerOnAWrittenChat(t *testing.T) {
	h := newHarness(t)
	h.loop.WithLight(&scriptedChat{})
	h.loop.fillGrace, h.loop.fillEvery = time.Millisecond, time.Millisecond
	h.loop.fillGraceWritten, h.loop.fillEveryWritten = time.Hour, time.Hour

	written := h.loop.fill(context.Background(), turnInput{sessionKey: "telegram:1", content: "y?", trigger: "user"})
	t.Cleanup(written.stop)
	spoken := h.loop.fill(context.Background(), turnInput{sessionKey: "voice:local", content: "y?", trigger: "user",
		notice: func(string) {}})
	t.Cleanup(spoken.stop)
	if written.grace != time.Hour || written.every != time.Hour {
		t.Errorf("written chat waits %v/%v", written.grace, written.every)
	}
	if spoken.grace != time.Millisecond || spoken.every != time.Millisecond {
		t.Errorf("spoken turn waits %v/%v", spoken.grace, spoken.every)
	}
}
