package agent

import (
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

// With somebody else in the room the model is told so. The memory scope keeps
// private recall out of the turn, but it cannot govern discretion in general:
// the model still holds the conversation's own history.
func TestChannelBriefingWarnsAboutCompany(t *testing.T) {
	private := channelBriefing("voice", "")
	shared := channelBriefing("voice", tools.AudienceShared)
	if !strings.HasPrefix(shared, private) {
		t.Errorf("the shared briefing dropped the spoken-reply guidance: %q", shared)
	}
	if !strings.Contains(shared, "besides the user is in the room") {
		t.Errorf("the shared briefing never mentions the room: %q", shared)
	}
	if strings.Contains(private, "in the room") {
		t.Errorf("a private turn was warned about company: %q", private)
	}

	// A channel with no briefing of its own gains none: a cron job has no
	// room, and the audience it reports would be meaningless.
	if got := channelBriefing("telegram", tools.AudienceShared); got != "" {
		t.Errorf("a written channel gained a room warning: %q", got)
	}
}
