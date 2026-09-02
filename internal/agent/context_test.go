package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// With somebody else in the room the model is told so. The memory scope keeps
// private recall out of the turn, but it cannot govern discretion in general:
// the model still holds the conversation's own history.
func TestChannelBriefingWarnsAboutCompany(t *testing.T) {
	private := channelBriefing(tools.ToolContext{Channel: "voice"})
	shared := channelBriefing(tools.ToolContext{Channel: "voice", Audience: tools.AudienceShared})
	if !strings.HasPrefix(shared, private) {
		t.Errorf("the shared briefing dropped the spoken-reply guidance: %q", shared)
	}
	if !strings.Contains(shared, "besides the user is in the room") {
		t.Errorf("the shared briefing never mentions the room: %q", shared)
	}
	// The notice argues for company from the strongest position in the
	// request, so it must carry the correction the sensor cannot make: the
	// user saying everyone has left settles it, via the room tool.
	if !strings.Contains(shared, "action=alone") || !strings.Contains(shared, "outranks") {
		t.Errorf("the shared briefing never grants the user's word the override: %q", shared)
	}
	if strings.Contains(private, "in the room") {
		t.Errorf("a private turn was warned about company: %q", private)
	}

	// A channel with no briefing of its own gains none: a cron job has no
	// room, and the audience it reports would be meaningless.
	if got := channelBriefing(tools.ToolContext{Channel: "telegram", Audience: tools.AudienceShared}); got != "" {
		t.Errorf("a written channel gained a room warning: %q", got)
	}
}

// The operating rules open the request, which is the strongest position a
// short prompt has and the weakest one a long conversation leaves them in:
// they never move, and every exchange added behind them pushes them toward
// the middle, where recall against a long input is worst. So past a certain
// size the tool rules are said again at the tail, where they are read.
func TestToolRulesAreRestatedOnceTheSessionIsLong(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	cb := NewContextBuilder(cfg, nil, nil)

	short := cb.TurnContext(context.Background(), nil, "hola", 0)
	if strings.Contains(short, "job_start") {
		t.Errorf("a session with nothing behind it was reminded of rules it had just read: %q", short)
	}

	// Eight chars a word, four bytes a token: enough words to put the head of
	// the prompt well past the point where it stops being read reliably.
	history := []provider.Message{{Role: "user", Content: strings.Repeat("palabra ", rulesFadeAt)}}
	long := cb.TurnContext(context.Background(), history, "hola", 0)
	if !strings.Contains(long, "job_start") || !strings.Contains(long, "browser tools") {
		t.Errorf("a long session was left with the rules it can no longer read: %q", long)
	}
	if !strings.HasSuffix(long, toolDiscipline) {
		t.Errorf("the restatement is not the last thing before the user's message: %q", long)
	}
}

// The failure this closes: far past rulesFadeAt the agent was asked to update
// itself, never reached for the upgrade tool, and hand-rolled a binary swap
// that killed its own gateway — which cancels the turn mid-step. The tool was
// in the request the whole time; what was missing was a rule naming which tool
// owns the job, in the one position a long session still reads.
func TestSelfUpgradeRuleSurvivesALongSession(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	cb := NewContextBuilder(cfg, nil, nil)

	if !strings.Contains(operatingRules, "upgrade tool") {
		t.Error("the rules at the head of the prompt no longer name the upgrade tool")
	}

	history := []provider.Message{{Role: "user", Content: strings.Repeat("palabra ", rulesFadeAt)}}
	long := cb.TurnContext(context.Background(), history, "actualizate", 0)
	if !strings.Contains(long, "upgrade tool") {
		t.Errorf("a long session lost the rule that changing the running Factor goes through the tool: %q", long)
	}
}

// The clash this exists to settle: on voice the only per-turn instruction was
// one telling the model to be brief and conversational, repeated every turn
// from the tail, arguing against stopping to use a tool. Both lines now ride
// the tail, and the tool rules come last.
func TestOnVoiceTheToolRulesComeAfterTheBrevityBriefing(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	cb := NewContextBuilder(cfg, nil, nil)

	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "voice"})
	history := []provider.Message{{Role: "user", Content: strings.Repeat("palabra ", rulesFadeAt)}}
	block := cb.TurnContext(ctx, history, "qué hora es", 0)

	brief := strings.Index(block, "spoken aloud on the user's speakers")
	rules := strings.Index(block, toolDiscipline)
	if brief < 0 || rules < 0 {
		t.Fatalf("the spoken turn is missing one of the two lines: %q", block)
	}
	if rules < brief {
		t.Errorf("the brevity briefing got the last word over the tool rules: %q", block)
	}
}

// A turn whose reply is delivered somewhere other than where it runs is
// briefed for that place too. The heartbeat's "system" session is nowhere the
// user reads, and the voice its reply comes out on speaks one language — the
// one thing a turn with no user message cannot work out for itself.
func TestBriefingCoversTheOutletAndItsLanguage(t *testing.T) {
	heartbeat := channelBriefing(tools.ToolContext{Channel: "system", Outlet: "voice", Language: "es"})
	for _, want := range []string{"spoken aloud on the user's speakers", `code "es"`} {
		if !strings.Contains(heartbeat, want) {
			t.Errorf("a heartbeat headed for the speakers was not told %q: %q", want, heartbeat)
		}
	}

	// A scheduled task keeps its own briefing and gains the outlet's.
	cron := channelBriefing(tools.ToolContext{Channel: "cron", Outlet: "voice", Language: "es"})
	for _, want := range []string{"nobody watching", "spoken aloud"} {
		if !strings.Contains(cron, want) {
			t.Errorf("a cron job headed for the speakers lost %q: %q", want, cron)
		}
	}

	// An outlet that is the channel itself is not briefed twice, and a
	// written outlet has nothing to add.
	if same := channelBriefing(tools.ToolContext{Channel: "voice", Outlet: "voice"}); strings.Count(same, "spoken aloud") != 1 {
		t.Errorf("a voice turn delivered to voice was briefed twice: %q", same)
	}
	if got := channelBriefing(tools.ToolContext{Channel: "system", Outlet: "telegram"}); got != "" {
		t.Errorf("a heartbeat headed for a written chat was briefed: %q", got)
	}
	// No fixed language, no sentence about one: the user's own is matched.
	if got := channelBriefing(tools.ToolContext{Channel: "voice"}); strings.Contains(got, "one language") {
		t.Errorf("a voice with no configured language was told it had one: %q", got)
	}
}
