package swarm

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

func echo(name string) func(agent.RemoteTurn) string {
	return func(rt agent.RemoteTurn) string { return name + " heard " + rt.Content }
}

// result is what an Invite reports once the other side has paired.
type result struct {
	name string
	err  error
}

func invite(t *testing.T, s *Service) (string, chan result) {
	t.Helper()
	codes := make(chan string, 1)
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := s.Invite(ctx, func(code string) error { codes <- code; return nil },
		func(name string, err error) { done <- result{name, err} })
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	return <-codes, done
}

func TestPairingByInvite(t *testing.T) {
	r := newRelay(t)
	desk, home := r.agent("desk", echo("desk")), r.agent("home box", echo("home"))
	code, done := invite(t, desk)
	if _, err := NormalizeCode(code); err != nil {
		t.Fatalf("the code %q does not parse: %v", code, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	name, err := home.Join(ctx, SpokenCode(code))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	t.Logf("pairing took %s once the code was entered", time.Since(start))
	if name != "desk" {
		t.Errorf("the newcomer calls the sponsor %q", name)
	}
	if res := <-done; res.err != nil || res.name != "home box" {
		t.Fatalf("the sponsor saw %+v", res)
	}
	if desk.Book().Read().Roster.Hash() != home.Book().Read().Roster.Hash() {
		t.Fatal("the two hold different rosters")
	}
	if res := relaySend(t, desk, "home box", "hi"); !strings.Contains(res.ForLLM, "home heard hi") {
		t.Errorf("after pairing: %q", res.ForLLM)
	}
	if res := relaySend(t, home, "desk", "hi"); !strings.Contains(res.ForLLM, "desk heard hi") {
		t.Errorf("the other way: %q", res.ForLLM)
	}

	// A third machine pairs with the newcomer, and the first learns of it.
	nas := r.agent("nas", echo("nas"))
	code, done = invite(t, home)
	if _, err := nas.Join(ctx, code); err != nil {
		t.Fatalf("third join: %v", err)
	}
	<-done
	waitFor(t, "the first member to learn of the third", func() bool { return desk.IsMember(nas.ID()) })
	waitFor(t, "its name", func() bool { return desk.Book().Read().Peer(nas.ID()).Name == "nas" })
	if res := relaySend(t, desk, "nas", "hi"); !strings.Contains(res.ForLLM, "nas heard hi") {
		t.Errorf("first to third: %q", res.ForLLM)
	}
}

func TestPairingTVStyle(t *testing.T) {
	r := newRelay(t)
	desk, fresh := r.agent("desk", echo("desk")), r.agent("new box", echo("new"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := desk.Found(ctx); err != nil {
		t.Fatal(err)
	}
	codes := make(chan string, 1)
	joined := make(chan result, 1)
	go func() {
		name, err := fresh.JoinByCode(ctx, r.url, func(code string) { codes <- code })
		joined <- result{name, err}
	}()
	name, err := desk.Join(ctx, WrittenCode(<-codes))
	if err != nil || name != "new box" {
		t.Fatalf("the member entering the code: %q %v", name, err)
	}
	if res := <-joined; res.err != nil || res.name != "desk" {
		t.Fatalf("the new machine: %+v", res)
	}
	if !fresh.IsMember(desk.ID()) || !desk.IsMember(fresh.ID()) {
		t.Error("not paired")
	}
}

func TestWrongCodeBurnsTheMailbox(t *testing.T) {
	r := newRelay(t)
	desk, home := r.agent("desk", echo("desk")), r.agent("home", echo("home"))
	code, done := invite(t, desk)
	n, p, _ := codeParts(code)
	wrong := n + "-" + p[:7] + string('0'+(p[7]-'0'+1)%10)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := home.Join(ctx, wrong); err == nil {
		t.Fatal("a wrong code paired")
	}
	if res := <-done; res.err == nil {
		t.Error("the sponsor did not see the failure")
	}
	if _, err := home.Join(ctx, code); err == nil {
		t.Error("the right code worked after a wrong guess")
	}
	if home.Book().Read().Joined() || len(desk.Book().Read().Roster.Members) != 1 {
		t.Error("membership changed after a failed pairing")
	}
}

func TestPairingRefusals(t *testing.T) {
	r := newRelay(t)
	a, b := r.agent("a", echo("a")), r.agent("b", echo("b"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Neither in a swarm: the machine entering the code cannot sponsor.
	codes := make(chan string, 1)
	go func() {
		if _, err := a.JoinByCode(ctx, r.url, func(code string) { codes <- code }); err != nil {
			close(codes)
		}
	}()
	code, ok := <-codes
	if !ok {
		t.Fatal("no code was made")
	}
	if _, err := b.Join(ctx, code); err == nil || !strings.Contains(err.Error(), "neither") {
		t.Errorf("neither in a swarm: %v", err)
	}
	// Both in swarms of their own.
	c, d := r.agent("c", echo("c")), r.agent("d", echo("d"))
	if err := d.Found(ctx); err != nil {
		t.Fatal(err)
	}
	code, done := invite(t, c)
	if _, err := d.Join(ctx, code); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("both in swarms: %v", err)
	}
	<-done
	if _, err := b.Join(ctx, "not a code"); err == nil {
		t.Error("joined with a non-code")
	}
	if _, err := b.JoinByCode(ctx, "", func(string) {}); err == nil {
		t.Error("made a code with no rendezvous")
	}
	n := newNode(t, "loner")
	if _, err := n.svc.Join(ctx, "1-23456789"); err == nil {
		t.Error("joined with no rendezvous configured")
	}
}

// scriptedAsk answers ask_user's way, recording what it was asked.
type scriptedAsk struct {
	mu     sync.Mutex
	answer string
	asked  []string
}

func (s *scriptedAsk) Ask(_ context.Context, q tools.Question) (tools.Answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, q.Prompt)
	return tools.Answer{Text: s.answer}, nil
}

func TestPairingThroughTheTool(t *testing.T) {
	r := newRelay(t)
	desk, home := r.agent("desk", echo("desk")), r.agent("home", echo("home"))
	deskBus, homeBus := desk.bus, home.bus
	chat := tools.ToolContext{Channel: "telegram", ChatID: "7", SessionKey: "telegram:7", Trigger: "user"}
	ctx := tools.WithToolContext(context.Background(), chat)

	// Telegram without an allow-list may not pair.
	desk.SetPairingGate(TelegramGate(json.RawMessage(`{"token":"x"}`)))
	if res := desk.Tools()[1].Execute(ctx, map[string]any{"action": "invite"}); !res.IsError ||
		!strings.Contains(res.ForLLM, "allow_from") {
		t.Errorf("open bot: %q", res.ForLLM)
	}
	desk.SetPairingGate(TelegramGate(json.RawMessage(`{"allow_from":["7"]}`)))

	res := desk.Tools()[1].Execute(ctx, map[string]any{"action": "invite"})
	if res.IsError || !strings.Contains(res.ForLLM, "run commands on this machine") {
		t.Fatalf("invite: %q", res.ForLLM)
	}
	msg := <-deskBus.Outbound()
	code, err := NormalizeCode(msg.Content)
	if err != nil || msg.ChatID != "7" {
		t.Fatalf("the code went out as %+v (%v)", msg, err)
	}
	if strings.Contains(res.ForLLM, WrittenCode(code)) || strings.Contains(res.ForLLM, code) {
		t.Fatal("the code reached the model")
	}

	// The new machine asks for the code itself and never shows it to the model.
	asker := &scriptedAsk{answer: SpokenCode(code)}
	home.SetAsker(func(context.Context) tools.Asker { return asker })
	res = home.Tools()[1].Execute(ctx, map[string]any{"action": "join", "question": "¿Cuál es el código?"})
	if res.IsError || !strings.Contains(res.ForLLM, `"desk"`) {
		t.Fatalf("join: %q", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, code) || asker.asked[0] != "¿Cuál es el código?" {
		t.Error("the join leaked the code or asked the wrong question")
	}
	report := <-deskBus.Inbound()
	if !report.System || !strings.Contains(report.Content, `"home"`) {
		t.Errorf("the sponsor's report: %+v", report)
	}
	_ = homeBus

	// A transcription that could mean two codes is refused, not guessed at.
	asker.answer = "forty seven"
	if res := home.Tools()[1].Execute(ctx, map[string]any{"action": "join"}); !res.IsError {
		t.Error("joined on an ambiguous transcription")
	}
	// Places a code cannot go.
	for _, ch := range []string{"cli", "phone"} {
		tc := chat
		tc.Channel = ch
		if res := desk.Tools()[1].Execute(tools.WithToolContext(context.Background(), tc),
			map[string]any{"action": "invite"}); !res.IsError {
			t.Errorf("invited from %s", ch)
		}
	}
	// Spoken, the code is read digit by digit.
	voice := chat
	voice.Channel, voice.ChatID = "voice", "local"
	if res := desk.Tools()[1].Execute(tools.WithToolContext(context.Background(), voice),
		map[string]any{"action": "invite"}); res.IsError {
		t.Fatalf("voice invite: %q", res.ForLLM)
	}
	if spoken := (<-deskBus.Outbound()).Content; !strings.Contains(spoken, ", ") {
		t.Errorf("spoken code %q", spoken)
	}
	nobody := newNode(t, "x")
	nobody.svc.bus = nil
	if res := nobody.svc.invite(context.Background(), chat, ""); !res.IsError {
		t.Error("invited with no bus")
	}
	if res := nobody.svc.join(context.Background(), chat, ""); !res.IsError {
		t.Error("joined with no asker")
	}
	_ = bus.External
}
