package wire

import (
	"strings"
	"testing"
)

func TestFrames(t *testing.T) {
	b := EncodeFrame(FrameData, 7, []byte("hi"))
	kind, sid, payload, err := DecodeFrame(b)
	if err != nil || kind != FrameData || sid != 7 || string(payload) != "hi" {
		t.Errorf("round trip: %d %d %q %v", kind, sid, payload, err)
	}
	if _, _, _, err := DecodeFrame([]byte{1, 2}); err == nil {
		t.Error("a short frame decoded")
	}
}

func TestHelloAndWork(t *testing.T) {
	k := newKey(t)
	sig := SignHello(k, "nonce")
	if pub, err := VerifyHello(EncodeKey(k.PublicKey()), "nonce", sig); err != nil || IDOf(pub) != k.ID() {
		t.Errorf("verify: %v", err)
	}
	if _, err := VerifyHello(EncodeKey(k.PublicKey()), "other", sig); err == nil {
		t.Error("a hello for another nonce verified")
	}
	if _, err := VerifyHello("junk", "nonce", sig); err == nil {
		t.Error("a junk key verified")
	}
	work, err := SolveWork("n", "subject", 8)
	if err != nil || !CheckWork("n", "subject", work, 8) || CheckWork("n", "other", work, 16) {
		t.Errorf("work %q %v", work, err)
	}
	if !CheckWork("n", "s", "", 0) || CheckWork("n", "s", strings.Repeat("9", 30), 1) {
		t.Error("work bounds")
	}
	if _, err := SolveWork("n", "s", MaxWorkBits+1); err == nil {
		t.Error("solved more work than an agent pays")
	}
	if len(NewSwarmID()) != 26 {
		t.Error("swarm id length")
	}
	if _, err := DecodeKey("bm9wZQ=="); err == nil {
		t.Error("a short key decoded")
	}
}
