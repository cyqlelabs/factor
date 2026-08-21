package voice

import (
	"fmt"
	"testing"
)

func TestEchoTrackerDiscardsAPureEcho(t *testing.T) {
	e := &echoTracker{}
	e.record("The weather in Madrid is sunny today.")
	rest, echo := e.strip("the weather in madrid is sunny today")
	if !echo {
		t.Error("the agent's own words came back and were not called echo")
	}
	if rest != "" {
		t.Errorf("rest = %q, want nothing left", rest)
	}
}

func TestEchoTrackerKeepsTheUserWordsAfterTheEcho(t *testing.T) {
	e := &echoTracker{}
	e.record("The weather in Madrid is sunny today.")
	rest, echo := e.strip("madrid is sunny today Factor stop talking")
	if echo {
		t.Error("an utterance carrying the user's words was called pure echo")
	}
	if rest != "Factor stop talking" {
		t.Errorf("rest = %q, want the user's words alone", rest)
	}
}

func TestEchoTrackerToleratesAGarbledWordMidEcho(t *testing.T) {
	e := &echoTracker{}
	e.record("the weather in madrid is sunny today")
	// The mic heard the speakers imperfectly: one word came back mangled.
	rest, echo := e.strip("the weather in madras is sunny today")
	if !echo {
		t.Errorf("one garbled word broke the echo match; rest = %q", rest)
	}
}

func TestEchoTrackerLeavesUnrelatedSpeechAlone(t *testing.T) {
	e := &echoTracker{}
	e.record("the weather in madrid is sunny today")
	rest, echo := e.strip("please open the window")
	if echo || rest != "please open the window" {
		t.Errorf("unrelated speech was touched: rest = %q, echo = %v", rest, echo)
	}
}

func TestEchoTrackerNeverEatsAShortCommand(t *testing.T) {
	e := &echoTracker{}
	e.record("i will stop the music right now")
	for _, cmd := range []string{"stop", "stop now"} {
		rest, echo := e.strip(cmd)
		if echo || rest != cmd {
			t.Errorf("strip(%q) = %q, %v — a short command must survive echo matching", cmd, rest, echo)
		}
	}
}

func TestEchoTrackerForgetsBeyondItsMemory(t *testing.T) {
	e := &echoTracker{}
	e.record("alpha beta gamma")
	for i := 0; i < echoMemoryWords; i++ {
		e.record(fmt.Sprintf("filler%d", i))
	}
	rest, echo := e.strip("alpha beta gamma")
	if echo || rest != "alpha beta gamma" {
		t.Errorf("words spoken long ago still count as echo: rest = %q, echo = %v", rest, echo)
	}
}
