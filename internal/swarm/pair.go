package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Codes never pass through the model. The agent that makes one sends it to
// the user itself and tells the model only that it did; the agent that takes
// one asks the user for it directly and hands it to the pairing unread. A
// model provider that saw a live code could claim the mailbox first.

// SetAsker is how the join action asks the user for a code without the
// model seeing the answer: the same routes ask_user takes.
func (s *Service) SetAsker(ask func(ctx context.Context) tools.Asker) { s.asker = ask }

// SetPairingGate adds a condition a channel must meet before it may pair:
// on Telegram, an allow_from list.
func (s *Service) SetPairingGate(gate func(channel string) error) { s.gate = gate }

// grantSentence says what a new member will be allowed to do here, which is
// said when a code is handed out or accepted.
func grantSentence() string {
	who := "the user Factor runs as"
	if u, err := user.Current(); err == nil {
		who = u.Username
		if u.Uid == "0" {
			who = "root"
		}
	}
	return fmt.Sprintf("It will be able to run commands on this machine, as %s.", who)
}

func (s *Service) mine() envelope {
	env := envelope{Name: s.Name()}
	if st := s.book.Read(); st.Joined() {
		env.Swarm, env.Rendezvous = st.Roster.Swarm, st.Roster.Home
	}
	return env
}

// conclude acts on a finished exchange: a member admits a newcomer, a
// newcomer waits to see itself on the roster. It returns what to call the
// other machine.
func (s *Service) conclude(ctx context.Context, theirs envelope) (string, error) {
	st := s.book.Read()
	name := strings.TrimSpace(theirs.Name)
	switch {
	case st.Joined() && theirs.Swarm == "":
		if err := s.Admit(ctx, theirs.Key, name); err != nil {
			return "", err
		}
		return name, nil
	case st.Joined() && theirs.Swarm == st.Roster.Swarm:
		return name, nil
	case st.Joined():
		return "", errors.New("both machines already belong to a swarm; remove one from its swarm first")
	case theirs.Swarm == "":
		return "", errors.New("neither machine is in a swarm; add the other machine from this one instead")
	}
	pub, err := DecodeKey(theirs.Key)
	if err != nil {
		return "", err
	}
	if err := s.book.Update(func(st *State) error {
		st.Joining, st.Rendezvous = theirs.Swarm, theirs.Rendezvous
		if name != "" {
			st.peer(IDOf(pub)).Name = name
		}
		return nil
	}); err != nil {
		return "", err
	}
	s.relink()
	deadline := time.Now().Add(time.Minute)
	for !s.book.Read().Joined() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", errors.New("paired, but the swarm's roster has not arrived yet; it will once this agent reaches the rendezvous")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return name, nil
}

// relink points the link at the rendezvous the swarm meets at, reconnecting
// if it was connected somewhere else or not at all.
func (s *Service) relink() {
	want := s.rendezvousURL()
	if _, link, _ := s.net(); link != nil && link.url == strings.TrimRight(want, "/") {
		return
	}
	s.disconnect()
	s.Connect()
}

// Invite makes a code for a new machine and hands it to deliver, which puts
// it in front of the user. The pairing then waits in the background, up to
// the code's lifetime, and report is told how it ended.
func (s *Service) Invite(ctx context.Context, deliver func(code string) error, report func(name string, err error)) error {
	if err := s.Found(ctx); err != nil {
		return err
	}
	p, err := makeCode(ctx, s.rendezvousURL())
	if err != nil {
		return err
	}
	if err := deliver(p.code); err != nil {
		_ = p.fail(err)
		return err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(s.bg, CodeTTL)
		defer cancel()
		theirs, err := p.exchange(ctx, s.id, s.mine())
		if err != nil {
			report("", err)
			return
		}
		name, err := s.conclude(ctx, theirs)
		if err == nil {
			p.done()
		}
		report(name, err)
	}()
	return nil
}

// Join enters a code another machine made: it joins that machine's swarm, or,
// when this agent is a member and the other machine is new, admits it.
func (s *Service) Join(ctx context.Context, code string) (string, error) {
	base := s.rendezvousURL()
	if base == "" {
		return "", errors.New("no rendezvous is configured (swarm.rendezvous)")
	}
	p, err := takeCode(ctx, base, code)
	if err != nil {
		return "", err
	}
	theirs, err := p.exchange(ctx, s.id, s.mine())
	if err != nil {
		return "", err
	}
	name, err := s.conclude(ctx, theirs)
	if err != nil {
		_ = p.fail(err)
		return "", err
	}
	p.done()
	return name, nil
}

// JoinByCode makes the code on this machine, for a member to enter, and
// waits for one to: the way a TV shows a code to link an account. factor init
// uses it on a new machine, where typing is easier on the member's side.
func (s *Service) JoinByCode(ctx context.Context, base string, show func(code string)) (string, error) {
	if base == "" {
		return "", errors.New("say which rendezvous to pair through")
	}
	if err := s.book.Update(func(st *State) error {
		st.Rendezvous = base
		return nil
	}); err != nil {
		return "", err
	}
	p, err := makeCode(ctx, base)
	if err != nil {
		return "", err
	}
	show(p.code)
	theirs, err := p.exchange(ctx, s.id, s.mine())
	if err != nil {
		return "", err
	}
	name, err := s.conclude(ctx, theirs)
	if err != nil {
		_ = p.fail(err)
		return "", err
	}
	p.done()
	return name, nil
}

// pairable refuses pairing from where it must not start.
func (s *Service) pairable(tc tools.ToolContext) error {
	if !bus.External(tc.Channel) {
		return errors.New("pairing starts from a chat or the microphone, where the code can reach the user")
	}
	if tc.Channel == "phone" {
		return errors.New("pairing over a phone call is not supported; use a chat or the microphone")
	}
	if s.gate != nil {
		return s.gate(tc.Channel)
	}
	return nil
}

// invite is the tool's invite action.
func (s *Service) invite(ctx context.Context, tc tools.ToolContext, _ string) *tools.Result {
	if err := s.pairable(tc); err != nil {
		return tools.Errorf("%v", err)
	}
	if s.bus == nil {
		return tools.Errorf("this agent has no way to send the code")
	}
	deliver := func(code string) error {
		text := WrittenCode(code)
		if tc.Channel == "voice" {
			text = SpokenCode(code)
		}
		if !s.bus.PublishOutbound(bus.OutboundMessage{Channel: tc.Channel, ChatID: tc.ChatID, Content: text}) {
			return errors.New("the code could not be sent: the outbound queue is full")
		}
		return nil
	}
	report := func(name string, err error) {
		content := fmt.Sprintf("[system] The new machine %q joined the swarm. Tell the user.", name)
		if err != nil {
			content = fmt.Sprintf("[system] Adding the new machine to the swarm failed: %v. Tell the user.", err)
		}
		s.bus.PublishInbound(bus.InboundMessage{Channel: tc.Channel, ChatID: tc.ChatID, Content: content,
			Time: s.now(), Audience: tc.Audience, System: true})
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.Invite(ctx, deliver, report); err != nil {
		return tools.Errorf("could not make a code: %v", err)
	}
	return tools.Textf("A pairing code went to the user directly; you do not see it. On the new machine they "+
		"turn the swarm on and say \"join the swarm\", then give it the code within %d minutes. Tell them that, "+
		"and this: %s", int(CodeTTL.Minutes()), grantSentence())
}

// join is the tool's join action: it asks the user for the code itself.
func (s *Service) join(ctx context.Context, tc tools.ToolContext, prompt string) *tools.Result {
	if err := s.pairable(tc); err != nil {
		return tools.Errorf("%v", err)
	}
	if s.asker == nil {
		return tools.Errorf("this agent has no way to ask for the code")
	}
	asker := s.asker(ctx)
	if asker == nil {
		return tools.Errorf("this agent has no way to ask for the code")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Swarm code?"
	}
	answer, err := asker.Ask(ctx, tools.Question{Prompt: prompt, Timeout: 3 * time.Minute})
	if err != nil {
		return tools.Errorf("no code came: %v", err)
	}
	if answer.Dismissed || strings.TrimSpace(answer.Text) == "" {
		return tools.Errorf("the user gave no code")
	}
	code, err := NormalizeCode(answer.Text)
	if err != nil {
		return tools.Errorf("the code was not understood (%v); ask the user to read it again digit by digit", err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	name, err := s.Join(ctx, code)
	if err != nil {
		return tools.Errorf("pairing failed: %v", err)
	}
	st := s.book.Read()
	return tools.Textf("Paired with %q: this agent and it are in one swarm of %d. %s", name, len(st.Roster.Members), grantSentence())
}

// TelegramGate refuses pairing over a Telegram bot that answers anyone: with
// no allow_from list, whoever finds the bot could add a machine to the swarm.
func TelegramGate(section json.RawMessage) func(channel string) error {
	return func(channel string) error {
		if channel != "telegram" {
			return nil
		}
		var tg struct {
			AllowFrom []string `json:"allow_from"`
		}
		_ = json.Unmarshal(section, &tg)
		if len(tg.AllowFrom) == 0 {
			return errors.New("pairing over Telegram needs channels.telegram.allow_from, so only the user's own chat can add a machine")
		}
		return nil
	}
}
