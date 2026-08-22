package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// gate is a provider round the test holds open, so a turn can be caught
// mid-flight with the session claimed — the state every scenario here is
// about.
type gate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

// hold is a scripted provider round that blocks until the test releases it.
func (g *gate) hold(content string) func(*provider.Request) (*provider.Response, error) {
	return func(*provider.Request) (*provider.Response, error) {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		<-g.release
		return &provider.Response{Content: content}, nil
	}
}

func (g *gate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the held turn never reached the provider")
	}
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// capture records every request the scripted provider was handed.
func lastRequest(h *harness) *provider.Request {
	h.chat.mu.Lock()
	defer h.chat.mu.Unlock()
	if len(h.chat.requests) == 0 {
		return nil
	}
	return h.chat.requests[len(h.chat.requests)-1]
}

func containsUser(req *provider.Request, want string) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		if msg.Role == "user" && strings.Contains(msg.Content, want) {
			return true
		}
	}
	return false
}

// A background job reporting back runs as a turn of its own on the session the
// user is speaking to. While it ran, what the user said next used to queue
// behind it with nothing logged and nothing said — the shape of the incident
// this steering path exists to prevent. It must reach the running turn
// instead, and the caller must be told there is nothing for it to say.
func TestSpokenTurnSteersIntoABackgroundJobsTurnInsteadOfQueuing(t *testing.T) {
	g := newGate()
	h := newHarness(t, g.hold(""), final("the job wrote the file, and we are on the last one"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	h.bus.PublishInbound(bus.InboundMessage{Channel: "voice", ChatID: "local",
		Content: "[system] Background job j8 finished with state done."})
	g.waitEntered(t)

	done := make(chan string, 1)
	go func() {
		reply, err := h.loop.ProcessDirectSteering(ctx, "y factor en que andamos", "voice:local", "Nicolás", "", nil)
		if err != nil {
			t.Errorf("ProcessDirectSteering: %v", err)
		}
		done <- reply
	}()

	// The point of the fix: the spoken turn is answered now, not when the
	// running one happens to finish.
	select {
	case reply := <-done:
		if reply != "" {
			t.Errorf("reply = %q, want nothing to say: the live turn answers", reply)
		}
	case <-time.After(2 * time.Second):
		g.open()
		t.Fatal("the spoken turn queued behind the running one instead of steering into it")
	}

	g.open()
	select {
	case out := <-h.bus.Outbound():
		if out.Channel != "voice" || out.ChatID != "local" {
			t.Errorf("outbound = %+v, want the voice chat", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never answered")
	}
	if req := lastRequest(h); !containsUser(req, "y factor en que andamos") {
		t.Error("the steered message never reached the model")
	}
}

// Who spoke travels with a steered message: on a shared machine the person
// who talks over a running turn is not always the one who started it.
func TestSteeredMessageCarriesItsSpeaker(t *testing.T) {
	g := newGate()
	h := newHarness(t, g.hold(""), final("noted"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	h.bus.PublishInbound(bus.InboundMessage{Channel: "voice", ChatID: "local", Content: "report"})
	g.waitEntered(t)
	// Bounded: a steering path that went back to queuing would hang here
	// rather than fail, which is a regression nobody reads.
	steer, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	if _, err := h.loop.ProcessDirectSteering(steer, "y esto?", "voice:local", "Roxana", "", nil); err != nil {
		g.open()
		t.Fatalf("the spoken turn did not steer: %v", err)
	}
	g.open()
	<-h.bus.Outbound()

	if req := lastRequest(h); !containsUser(req, "[Roxana] y esto?") {
		t.Error("the steered message lost the name of who said it")
	}
}

// The incident had two utterances, and the second cancelled the first while it
// was still waiting for a claim it never got. Steered, both are answered.
func TestEverySpokenMessageDuringABusyTurnReachesTheModel(t *testing.T) {
	g := newGate()
	h := newHarness(t, g.hold(""), final("both answered"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	h.bus.PublishInbound(bus.InboundMessage{Channel: "voice", ChatID: "local", Content: "report"})
	g.waitEntered(t)
	for _, said := range []string{"en que andamos", "estás ahí?"} {
		steer, stop := context.WithTimeout(ctx, 2*time.Second)
		if _, err := h.loop.ProcessDirectSteering(steer, said, "voice:local", "", "", nil); err != nil {
			stop()
			g.open()
			t.Fatalf("%q did not steer: %v", said, err)
		}
		stop()
	}
	g.open()
	<-h.bus.Outbound()

	req := lastRequest(h)
	for _, said := range []string{"en que andamos", "estás ahí?"} {
		if !containsUser(req, said) {
			t.Errorf("%q never reached the model", said)
		}
	}
}

// A connector whose reply has to come back on the medium it arrived on — a
// phone call the user is holding — must still own its turn. Steering it would
// answer through the bus, which for the phone means dialling a second call.
func TestATurnThatMustOwnItsSessionStillWaits(t *testing.T) {
	g := newGate()
	h := newHarness(t, g.hold("the job is done"), final("still here"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	h.bus.PublishInbound(bus.InboundMessage{Channel: "phone", ChatID: "+15550100", Content: "report"})
	g.waitEntered(t)

	done := make(chan string, 1)
	go func() {
		reply, err := h.loop.ProcessDirectNotice(ctx, "are you there?", "phone:+15550100", "", "", func(string) {})
		if err != nil {
			t.Errorf("ProcessDirectNotice: %v", err)
		}
		done <- reply
	}()

	select {
	case reply := <-done:
		g.open()
		t.Fatalf("the call's turn returned %q while the session was busy", reply)
	case <-time.After(500 * time.Millisecond):
	}

	g.open()
	select {
	case reply := <-done:
		if reply != "still here" {
			t.Errorf("reply = %q, want the call's own answer", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call's turn never ran")
	}
}

// Confidentiality outranks promptness. A turn claimed for a private
// conversation recalls private memory, so a message somebody else can hear
// must not be folded into it — it waits and gets a turn scoped to the room.
func TestSteeringIsRefusedWhenTheRoomGrewSinceTheTurnStarted(t *testing.T) {
	g := newGate()
	h := newHarness(t, g.hold("job done"), final("answered to the room"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	// The job's turn carries no audience: it was claimed for a private chat.
	h.bus.PublishInbound(bus.InboundMessage{Channel: "voice", ChatID: "local", Content: "report"})
	g.waitEntered(t)

	done := make(chan string, 1)
	go func() {
		reply, err := h.loop.ProcessDirectSteering(ctx, "what is on my calendar?", "voice:local", "",
			tools.AudienceShared, nil)
		if err != nil {
			t.Errorf("ProcessDirectSteering: %v", err)
		}
		done <- reply
	}()

	select {
	case reply := <-done:
		g.open()
		t.Fatalf("a shared-room message steered into a private turn (reply %q)", reply)
	case <-time.After(500 * time.Millisecond):
	}

	g.open()
	select {
	case reply := <-done:
		if reply != "answered to the room" {
			t.Errorf("reply = %q, want a turn of its own", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the shared turn never ran")
	}
}

// The other direction costs nothing: the guest left, so the message is private
// again while the running turn is still scoped to the room. Nobody hears
// anything they could not already hear, so it steers.
func TestAPrivateMessageStillSteersIntoASharedTurn(t *testing.T) {
	g := newGate()
	h := newHarness(t, g.hold(""), final("done"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	h.bus.PublishInbound(bus.InboundMessage{Channel: "voice", ChatID: "local",
		Content: "report", Audience: tools.AudienceShared})
	g.waitEntered(t)

	steer, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	reply, err := h.loop.ProcessDirectSteering(steer, "and now?", "voice:local", "", "", nil)
	if err != nil {
		g.open()
		t.Fatalf("a private message did not steer into a shared turn: %v", err)
	}
	if reply != "" {
		t.Errorf("reply = %q, want nothing to say: the live turn answers", reply)
	}
	g.open()
	<-h.bus.Outbound()
	if req := lastRequest(h); !containsUser(req, "and now?") {
		t.Error("the steered message never reached the model")
	}
}

// A steering queue with no room left is the one case where the caller must go
// back to waiting: returning as though the message had landed would lose it
// with no reply and no trace, which is the failure this whole path is about.
func TestAFullSteeringQueueMakesTheSpokenTurnWaitInsteadOfVanishing(t *testing.T) {
	h := newHarness(t, final("answered at last"))
	key := "voice:local"
	held, ok, _ := h.loop.claim(key, &bus.InboundMessage{Channel: "voice", ChatID: "local"}, false)
	if !ok {
		t.Fatal("claim failed on an idle session")
	}
	for i := range steeringBuffer {
		h.loop.claim(key, &bus.InboundMessage{Channel: "voice", ChatID: "local",
			Content: string(rune('a' + i))}, true)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan string, 1)
	go func() {
		reply, err := h.loop.ProcessDirectSteering(ctx, "one more", key, "", "", nil)
		if err != nil {
			t.Errorf("ProcessDirectSteering: %v", err)
		}
		done <- reply
	}()

	select {
	case reply := <-done:
		t.Fatalf("the message was dropped rather than waited out (reply %q)", reply)
	case <-time.After(500 * time.Millisecond):
	}

	h.loop.release(key, held)
	select {
	case reply := <-done:
		if reply != "answered at last" {
			t.Errorf("reply = %q, want the turn it finally owned", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting turn never ran")
	}
}

// A note the agent makes on its way to an answer reaches whoever is running
// the turn directly. Publishing it as well makes a connector that hears both
// say it twice, so the bus copy is for turns nobody is holding.
func TestANoteGoesOutOnceToWhoeverIsRunningTheTurn(t *testing.T) {
	h := newHarness(t,
		func(*provider.Request) (*provider.Response, error) {
			return &provider.Response{Content: "one moment",
				ToolCalls: []provider.ToolCall{{ID: "tc1", Name: "probe", Args: map[string]any{"value": "x"}}}}, nil
		},
		final("here you go"))
	seen := collect(t, h)

	var notes []string
	if _, err := h.loop.ProcessDirectNotice(context.Background(), "look it up", "voice:local", "", "",
		func(line string) { notes = append(notes, line) }); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0] != "one moment" {
		t.Errorf("notes = %q, want the line delivered to the turn's own listener", notes)
	}
	for _, act := range *seen {
		if act.Phase == PhaseNotice {
			t.Error("the note was also published off the bus; the connector would say it twice")
		}
	}
}

// A turn nobody is holding — a job reporting back — has no other way to make
// progress audible, so its notes do go out on the bus.
func TestATurnNobodyIsHoldingPublishesItsNotes(t *testing.T) {
	h := newHarness(t,
		func(*provider.Request) (*provider.Response, error) {
			return &provider.Response{Content: "writing the file",
				ToolCalls: []provider.ToolCall{{ID: "tc1", Name: "probe", Args: map[string]any{"value": "x"}}}}, nil
		},
		final("done"))
	seen := collect(t, h)

	if _, err := h.loop.ProcessDirect(context.Background(), "[system] job finished", "voice:local"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, act := range *seen {
		if act.Phase == PhaseNotice && act.Detail == "writing the file" {
			found = true
		}
	}
	if !found {
		t.Errorf("no note published in %v", phases(*seen))
	}
}
