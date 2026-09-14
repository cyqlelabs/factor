package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

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

	// fillerDeadline bounds the call. A filler is worth having only while the
	// user is still wondering; past this it is noise arriving on top of the
	// answer, and saying nothing is better.
	fillerDeadline = 6 * time.Second

	// fillerMaxTokens is a dozen words and the punctuation around them.
	fillerMaxTokens = 64

	// fillerToolsShown bounds what the prompt says has happened so far.
	// Twenty iterations of a long turn is a list nobody reads and the model
	// only needs the shape of.
	fillerToolsShown = 6
)

// filler tracks one turn's silence and breaks it. Every field behind mu is
// written by the turn and read by the goroutine composing the next line.
type filler struct {
	loop *Loop
	in   turnInput

	said chan struct{}
	done chan struct{}
	once sync.Once

	mu     sync.Mutex
	tools  []string // the tool calls this turn has finished, in order
	flight []string // the batch running right now
	lines  []string // what the filler has already said, so it does not repeat
}

// fill starts the filler for one turn. It says nothing until the turn has been
// quiet for fillerGrace, and stops the moment the turn has an answer.
//
// Only a turn somebody is waiting on gets one. A heartbeat, a cron job and a
// delegated job all run with nobody holding the line, and their notices are
// published to whichever chat the user last used — so a filler there is not
// company, it is the machine interrupting an empty room to say it is busy.
func (l *Loop) fill(ctx context.Context, in turnInput) *filler {
	f := &filler{loop: l, in: in, said: make(chan struct{}, 1), done: make(chan struct{})}
	if in.ephemeral || in.trigger != "user" {
		f.stop()
		return f
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

// stop ends the filler, and is called once: the turn's own cancellation ends
// the goroutine on every other path out.
func (f *filler) stop() { f.once.Do(func() { close(f.done) }) }

func (f *filler) run(ctx context.Context) {
	wait := f.loop.fillGrace
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
		wait = f.loop.fillEvery
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
			{Role: "system", Content: fillerRules(f.in.toolCtx.Language)},
			{Role: "user", Content: fillerState(f.in.content, done, said)},
		},
		MaxTokens: fillerMaxTokens,
	})
	if err != nil {
		slog.Debug("filler line unavailable", "session", f.in.sessionKey, "error", err)
		f.loop.lightFault.Do(func() {
			slog.Warn("the fast model could not write a line while a turn worked; turns will run quiet",
				"error", err, "fix", "set provider.light.model to a model this account can reach")
		})
		return ""
	}
	return firstLine(resp.Content)
}

// fillerRules is the whole of the filler's brief. Every clause of it is a
// refusal: the one failure that matters here is a fast model guessing at the
// answer the slow one is still working out, and having that guess spoken as
// though it were the reply.
func fillerRules(language string) string {
	rules := "You are the voice of an assistant that is in the middle of working on something for the user. " +
		"Say one short sentence — under twelve words — telling them what you are doing right now. " +
		"You are not answering: you do not know the answer, nobody is asking you for it, and the assistant will give it itself in a moment. " +
		"Never answer or guess at the request. Never greet, apologise, or say how long anything will take. " +
		"Never use markdown, lists or quotation marks. Reply with the sentence and nothing else."
	if language != "" {
		return rules + fmt.Sprintf(" Write it in the language with code %q, whatever language this request is in.", language)
	}
	return rules + " Write it in the same language the user used."
}

// fillerState is what the filler knows: what was asked, what has been done
// about it, and what has already been said out loud. Tool names travel, their
// arguments do not — the user's paths and searches are no business of a
// second model, and the shape of the work is all the sentence needs.
func fillerState(request string, done, said []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The user said: %q\n", firstLine(request))
	if len(done) > fillerToolsShown {
		done = done[len(done)-fillerToolsShown:]
	}
	if len(done) > 0 {
		fmt.Fprintf(&b, "Tools run so far, oldest first: %s\n", strings.Join(done, ", "))
	} else {
		b.WriteString("No tool has run yet; the assistant is still working out what to do.\n")
	}
	if len(said) > 0 {
		fmt.Fprintf(&b, "You have already said: %q. Do not say it again.\n", strings.Join(said, " / "))
	}
	b.WriteString("Say the next line.")
	return b.String()
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
