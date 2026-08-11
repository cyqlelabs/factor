package telegram

import (
	"context"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/bus"
)

const leakTestToken = "987654:ANOTHER-SECRET-BOT-TOKEN"

// A malformed api_base makes request construction fail, and that error
// embeds the full URL — including the bot token.
func TestRequestBuildErrorsNeverLeakTheToken(t *testing.T) {
	tg, err := New(Config{Token: leakTestToken, APIBase: "http://%zz"}, bus.New())
	if err != nil {
		t.Fatal(err)
	}

	_, pollErr := tg.getUpdates(context.Background())
	if pollErr == nil {
		t.Fatal("expected getUpdates to fail on a malformed api_base")
	}
	if strings.Contains(pollErr.Error(), leakTestToken) {
		t.Errorf("getUpdates leaked the bot token: %v", pollErr)
	}

	sendErr := tg.Send(context.Background(), bus.OutboundMessage{Channel: "telegram", ChatID: "1", Content: "hi"})
	if sendErr == nil {
		t.Fatal("expected Send to fail on a malformed api_base")
	}
	if strings.Contains(sendErr.Error(), leakTestToken) {
		t.Errorf("Send leaked the bot token: %v", sendErr)
	}
}
