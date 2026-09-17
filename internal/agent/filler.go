package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// The filler is what the conversation says while a turn is still working.
//
// The rules ask the model to open with a line before its first tool call, and
// the notice path carries that line the moment it exists — but a prompt is a
// request. Captured over a long spoken session, five of six tool-calling
// iterations came back with the call and no text, so there was nothing to
// deliver and the room stayed silent for two minutes.
//
// A canned line closed that hole and opened another one: heard on every turn,
// "give me a moment" is an on-hold message, and an on-hold message says the
// same nothing however long the wait runs. What a person says instead is what
// they are doing — "let me pull up your last session" — which is a sentence
// only something that can read the turn can write.
//
// So the line is written by a model, and by the cheapest, fastest one this
// install can reach rather than the one the user is already waiting on. The
// request is a couple of hundred tokens against the conversation's fifty
// thousand, it thinks about nothing, and it is never allowed near the answer:
// it reports what is happening and stops.
const (
	// fillerGrace is how long a turn may run in silence before the filler
	// speaks, and fillerInterval how often it speaks after that.
	//
	// The grace is set by what a pause means to a person, not by what the
	// chain costs. Three seconds of nothing is already long enough in a room
	// and past the point where a caller checks whether the line dropped, and
	// a person who is about to go and look something up says so about that
	// fast. There is no longer a reason to wait out an ordinary answer
	// either: the line is written about this turn rather than stamped out,
	// so hearing one before a quick reply is a conversation rather than a
	// tic — and an answer that beats the filler to the speakers cancels it
	// outright rather than queuing behind it. See speak.
	fillerGrace    = 3 * time.Second
	fillerInterval = 30 * time.Second

	// fillerGraceWritten and fillerIntervalWritten are the same two numbers
	// for a written chat, where each line is a message that stays in the
	// thread and, on a phone, a notification. A written channel is not
	// silent while a turn works — Telegram shows the typing indicator the
	// whole time — so the filler there is not the difference between alive
	// and dead, only between busy and busy-at-what. At the spoken cadence
	// one fifteen-minute Telegram turn produced twenty of them, three
	// between each line the model wrote itself, which read as noise rather
	// than company.
	fillerGraceWritten    = 20 * time.Second
	fillerIntervalWritten = 2 * time.Minute

	// fillerDeadline bounds the call, and only against a request that hangs.
	// It was six seconds when it also had to keep a late line off the
	// answer; that is now done by dropping a line the turn has outrun and
	// cancelling the call the moment the answer lands, so the deadline can
	// afford to be honest about the model. Measured on the default route the
	// call takes four to six seconds and more, and at six the deadline was
	// cancelling every other filler two seconds before it would have spoken
	// — into turns that then ran another thirty seconds in silence.
	fillerDeadline = 15 * time.Second

	// fillerMaxTokens is a dozen words, the punctuation around them, and room
	// for a model to think first. The sentence itself is twenty tokens; the
	// rest is headroom, because on the OpenAI dialects this cap covers the
	// reasoning too and a cap spent thinking returns finish_reason "length"
	// with no content at all. Sized at a dozen words it did exactly that on
	// every call, which is a filler that never spoke.
	fillerMaxTokens = 512

	// fillerToolsShown bounds what the prompt says has happened so far.
	// Twenty iterations of a long turn is a list nobody reads and the model
	// only needs the shape of.
	fillerToolsShown = 6

	// fillerRequestChars bounds how much of the request travels. A minute of
	// talking is one subject and the filler needs the subject.
	fillerRequestChars = 240

	// fillerLanguageSample is how many of the user's recent messages decide
	// the language of a line on a channel that names none. One is not
	// enough: "y?" says nothing about what language it was asked in, and the
	// fifteen minutes of English lines that followed it were spoken into a
	// Spanish conversation.
	fillerLanguageSample = 5
)

// filler tracks one turn's silence and breaks it. Every field behind mu is
// written by the turn and read by the goroutine composing the next line.
type filler struct {
	loop *Loop
	in   turnInput

	said chan struct{}
	done chan struct{}
	once sync.Once

	grace, every time.Duration

	mu       sync.Mutex
	tools    []string // the tool calls this turn has finished, in order
	flight   []string // the batch running right now
	lines    []string // what the filler has already said, so it does not repeat
	language string   // the language the line comes out in; blank means the request's
}

// fill starts the filler for one turn. It says nothing until the turn has been
// quiet for fillerGrace, and stops the moment the turn has an answer.
//
// Only a turn somebody is waiting on gets one. A heartbeat, a cron job and a
// delegated job all run with nobody holding the line, and their notices are
// published to whichever chat the user last used — so a filler there is not
// company, it is the machine interrupting an empty room to say it is busy.
func (l *Loop) fill(ctx context.Context, in turnInput) *filler {
	f := &filler{loop: l, in: in, said: make(chan struct{}, 1), done: make(chan struct{}),
		grace: l.fillGrace, every: l.fillEvery, language: in.toolCtx.Language}
	if in.ephemeral || in.trigger != "user" {
		f.stop()
		return f
	}
	// A turn with nowhere to hand its notices to is a written chat, read off
	// the bus: its lines are messages rather than a voice in a room.
	if in.notice == nil {
		f.grace, f.every = l.fillGraceWritten, l.fillEveryWritten
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		f.run(ctx)
	}()
	return f
}

// spoke restarts the clock: the turn has just said something of its own, so
// the filler owes nothing for another interval.
func (f *filler) spoke() {
	select {
	case f.said <- struct{}{}:
	default:
	}
}

// running records the batch about to go out, and finished moves it into the
// turn's history once it comes back. What the turn is doing is the whole
// substance of what the filler has to say, and a batch still in flight is the
// part of it that is actually now.
func (f *filler) running(names ...string) {
	f.mu.Lock()
	f.flight = names
	f.mu.Unlock()
}

func (f *filler) finished() {
	f.mu.Lock()
	f.tools, f.flight = append(f.tools, f.flight...), nil
	f.mu.Unlock()
}

// learn reads the language the conversation is held in off what the user
// has written so far, for a channel that does not name one. The rule
// "write it in the language of the request" fails on exactly the requests a
// person sends mid-task — "y?", "ok", "dale" — and the examples in the brief
// then decide, which is how a Spanish conversation got fifteen minutes of
// English. A channel that names a language keeps it: the voice reading the
// line speaks that one whatever the user typed.
func (f *filler) learn(history []provider.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.language != "" {
		return
	}
	f.language = conversationLanguage(f.in.content, history)
}

// stop ends the filler, and is called once: the turn's own cancellation ends
// the goroutine on every other path out.
func (f *filler) stop() { f.once.Do(func() { close(f.done) }) }

func (f *filler) run(ctx context.Context) {
	wait := f.grace
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-f.done:
			timer.Stop()
			return
		case <-f.said:
			timer.Stop()
		case <-timer.C:
			f.speak(ctx)
		}
		wait = f.every
	}
}

// speak composes one line and delivers it the way the turn's own notices go
// out. A failure is silence rather than a fault: nothing here is part of the
// answer, and a turn must never end because its filler could not be written.
//
// Composing takes a call, and the answer can land while it is in flight. What
// the channel does with the two then is not overlap — a spoken reply holds
// the floor for its whole length, and the phone's stream serializes its
// writers — it is order, and "I'm looking that up" arriving behind the thing
// it was looking up is worse than silence. So a line the turn has outrun is
// dropped where it stands.
func (f *filler) speak(ctx context.Context) {
	line := f.compose(ctx)
	if line == "" {
		return
	}
	select {
	case <-f.done:
		return
	default:
	}
	f.mu.Lock()
	f.lines = append(f.lines, line)
	f.mu.Unlock()
	// The line is not persisted. It is the channel keeping the user company,
	// not the agent speaking, and a transcript that holds it would offer it
	// back the next time somebody asks what was said.
	if f.in.notice != nil {
		f.in.notice(line)
		return
	}
	f.loop.emit(f.in.sessionKey, PhaseNotice, line)
}

func (f *filler) compose(ctx context.Context) string {
	chat := f.loop.lightChat()
	if chat == nil {
		return ""
	}
	f.mu.Lock()
	done := append(append([]string(nil), f.tools...), f.flight...)
	said, waiting := append([]string(nil), f.lines...), slices.Contains(f.flight, tools.AskToolName)
	language := f.language
	f.mu.Unlock()
	// The turn has asked the user something and is waiting on them. The
	// silence is theirs to break, and filling it is talking over an answer.
	if waiting {
		return ""
	}

	// The deadline hangs off the turn, so a cancelled turn cancels the filler
	// it no longer needs — and so does a turn that simply finished, since
	// there is nothing left to say and no reason to pay for the saying.
	ctx, cancel := context.WithTimeout(ctx, fillerDeadline)
	defer cancel()
	go func() {
		select {
		case <-f.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	resp, err := chat.Chat(ctx, &provider.Request{
		Messages: []provider.Message{
			{Role: "system", Content: fillerRules(language)},
			{Role: "user", Content: fillerState(f.in.content, done, said, language)},
		},
		MaxTokens: fillerMaxTokens,
	})
	if err != nil {
		slog.Debug("filler line unavailable", "session", f.in.sessionKey, "error", err)
		f.loop.lightFault.Do(func() {
			slog.Warn("the fast model could not write a line while a turn worked; turns will run quiet",
				"error", err, "chain", "provider.light")
		})
		return ""
	}
	return firstLine(resp.Content)
}

// fillerRules is the filler's brief, built the way a brief for a small fast
// model has to be: the job, the handful of rules that decide whether the line
// is usable at all, and then demonstrations — a model this size follows a
// worked example much further than it follows a paragraph about one.
//
// The one failure that matters here is a fast model guessing at the answer
// the slow one is still working out, and having that guess spoken as though
// it were the reply. Saying so is worth a line; showing it three times is
// what actually holds, which is why the examples pair a request with a line
// that conspicuously does not answer it.
//
// The rules are stated in the positive. "Never do X" leaves a small model
// holding X, and the prohibitions this replaced were five of the eight
// clauses in the brief.
func fillerRules(language string) string {
	var b strings.Builder
	b.WriteString("<role>\n" +
		"You are the voice of an assistant in the middle of a task, for someone who is waiting and has heard nothing yet. " +
		"Your whole job is one short sentence saying what is happening right now.\n" +
		"</role>\n\n<rules>\n" +
		"- One sentence, under twelve words, spoken plainly as the assistant would say it out loud.\n" +
		"- Say what is being done. The answer belongs to the assistant, who will give it in a moment: you do not know it and nobody is asking you for it.\n" +
		"- Reply with the sentence and nothing else — no quotation marks, no markdown, no preamble.\n")
	b.WriteString("- " + fillerLanguageRule(language) + "\n</rules>\n\n<examples>\n")
	b.WriteString(fillerExamples(language))
	b.WriteString("</examples>")
	return b.String()
}

// fillerLanguageRule fixes the language of the line. A spoken outlet names
// one and means it: the voice reading the reply speaks that language and
// nothing else, so a line in the wrong one is noise in an accent.
func fillerLanguageRule(language string) string {
	if language == "" {
		return "Write it in the same language as the request."
	}
	return fmt.Sprintf("Write it in the language with code %q, whatever language the request is in.", language)
}

// fillerExamples demonstrates the shape: a request, what is under way, and a
// line about the second that leaves the first alone. They are written in the
// language the line has to come out in, since an example is the strongest
// thing in the prompt and one in the wrong language pulls the answer with it.
func fillerExamples(language string) string {
	if strings.HasPrefix(strings.ToLower(language), "es") {
		return "" +
			"Request: ¿cómo está el clima en Rosario? · running: web_search\n" +
			"Estoy mirando el pronóstico.\n\n" +
			"Request: ¿qué decía el informe que te pasé? · running: read_file\n" +
			"Estoy abriendo el informe.\n\n" +
			"Request: fijate si hay pasajes para el viernes · running: browser_navigate, browser_read\n" +
			"Estoy revisando la página.\n"
	}
	return "" +
		"Request: what is the weather in Rosario? · running: web_search\n" +
		"I'm checking the forecast.\n\n" +
		"Request: what did the report I sent you say? · running: read_file\n" +
		"I'm opening the report.\n\n" +
		"Request: see if there are flights on Friday · running: browser_navigate, browser_read\n" +
		"I'm going through the page.\n"
}

// fillerState is what the filler knows: what was asked, what has been done
// about it, and what has already been said out loud. Tool names travel, their
// arguments do not — a second model has no business with the user's paths and
// searches, and the shape of the work is all the sentence needs.
//
// It closes with the instruction rather than with the request. Recall is
// strongest at the two ends of a prompt, and the request is the one thing
// here that pulls toward answering: ending on it put the temptation in the
// best seat in the house and the rule against it in the worst.
func fillerState(request string, done, said []string, language string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<request>\n%s\n</request>\n\n", clipLine(request, fillerRequestChars))
	if len(done) > fillerToolsShown {
		done = done[len(done)-fillerToolsShown:]
	}
	b.WriteString("<progress>\n")
	if len(done) > 0 {
		fmt.Fprintf(&b, "Tools run so far, oldest first: %s\n", strings.Join(done, ", "))
	} else {
		b.WriteString("No tool has run yet; the assistant is still working out what to do.\n")
	}
	b.WriteString("</progress>\n\n")
	if len(said) > 0 {
		fmt.Fprintf(&b, "<already_said>\n%s\n</already_said>\nSay something else.\n\n", strings.Join(said, "\n"))
	}
	fmt.Fprintf(&b, "Write the line now: one sentence, under twelve words, about what is happening — not the answer. %s",
		fillerLanguageRule(language))
	return b.String()
}

// conversationLanguage guesses the language a written conversation is held
// in from the request and the user's last few messages, for the one
// distinction the brief can act on: Spanish, or not. It counts function
// words that belong to one language and not the other — "the" and "que" do
// not cross over — and the characters Spanish has and English lacks, so a
// two-letter request is read in the light of the sentences before it. It
// is a guess and says so by returning nothing rather than a wrong code:
// unknown gets the request's own language and the English examples, which
// is what every conversation got before.
func conversationLanguage(request string, history []provider.Message) string {
	texts := []string{request}
	for i := len(history) - 1; i >= 0 && len(texts) <= fillerLanguageSample; i-- {
		m := history[i]
		// A job's completion is filed as the user speaking and is not: it
		// is machinery, and it is written in English.
		if m.Role != "user" || strings.HasPrefix(m.Content, "[system]") {
			continue
		}
		texts = append(texts, m.Content)
	}
	es, en := 0, 0
	for _, t := range texts {
		for _, r := range t {
			if strings.ContainsRune("¿¡ñáéíóúÁÉÍÓÚÑ", r) {
				es++
			}
		}
		for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool { return !unicode.IsLetter(r) }) {
			switch {
			case spanishWords[w]:
				es++
			case englishWords[w]:
				en++
			}
		}
	}
	if es > en {
		return "es"
	}
	return ""
}

// spanishWords and englishWords are function words that occur in one of the
// two languages and not the other. Words both use — "no", "me", "a" — are
// left out on purpose, since they would vote for both sides at once.
var (
	spanishWords = wordSet("el la los las de del que qué y en un una es está están para con por pero " +
		"cómo más hay se lo al te ya muy también esto eso esta este ese esa porque cuando dónde donde " +
		"todo nada bien ahora sí acá ahí dale sigue fijate")
	englishWords = wordSet("the and is are was were what how with for this that these those you your it " +
		"to of in on do does did can could please check my not be have has from at by there here when " +
		"where why which if then also just now all")
)

func wordSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(words) {
		set[w] = true
	}
	return set
}

// clipLine bounds one line of somebody else's text. A request runs as long as
// the user felt like talking, and the filler needs its subject rather than
// all of it.
func clipLine(s string, limit int) string {
	s = firstLine(s)
	if len(s) <= limit {
		return s
	}
	return strings.TrimSpace(s[:limit]) + "…"
}

// firstLine trims a reply to one spoken sentence's worth of text. A model
// told to answer with one line occasionally answers with two.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// toolNames lists what a batch of calls asked for, in the order the model
// asked for it.
func toolNames(calls []provider.ToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}
