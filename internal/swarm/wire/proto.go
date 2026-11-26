// Package wire is what a swarm's agents and its rendezvous both speak: the
// roster, the keys it lists, and the messages on the agent socket.
package wire

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
)

// The agent socket carries two kinds of WebSocket message: JSON text for
// control (authentication, the roster, presence) and binary frames for the
// relayed byte streams between two members.

// Control is one control message. Ref ties a reply to its request.
type Control struct {
	Type   string   `json:"type"`
	Ref    string   `json:"ref,omitempty"`
	Nonce  string   `json:"nonce,omitempty"`
	Bits   int      `json:"bits,omitempty"`
	Key    string   `json:"key,omitempty"`
	Sig    string   `json:"sig,omitempty"`
	Work   string   `json:"work,omitempty"`
	Swarm  string   `json:"swarm,omitempty"`
	ID     string   `json:"id,omitempty"`
	Roster *Roster  `json:"roster,omitempty"`
	Online []string `json:"online,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// Control types.
const (
	CtlChallenge = "challenge" // server: a nonce to sign, and the work a new key owes
	CtlHello     = "hello"     // agent: its key, the signed nonce, the work, its swarm
	CtlWelcome   = "welcome"   // server: the agent's id and its swarm's roster
	CtlCommit    = "commit"    // agent: the next roster
	CtlCommitted = "committed" // server: the roster is the swarm's new head
	CtlRoster    = "roster"    // server: the current roster, pushed or asked for
	CtlFetch     = "fetch"     // agent: send me the roster
	CtlPresence  = "presence"  // agent asks, server answers: who is online
	CtlError     = "error"
)

// Frame types on the relay.
const (
	FrameOpen   byte = 1 // payload: the agent id the stream is to (agent) or from (server)
	FrameData   byte = 2
	FrameClose  byte = 3 // payload: why, if anything
	FrameCredit byte = 4 // payload: uint32 bytes the receiver will take
)

// MaxFrame bounds one relayed frame's payload.
const MaxFrame = 16 << 10

// EncodeFrame writes a relay frame.
func EncodeFrame(kind byte, sid uint32, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = kind
	binary.BigEndian.PutUint32(out[1:5], sid)
	copy(out[5:], payload)
	return out
}

// DecodeFrame reads one.
func DecodeFrame(b []byte) (kind byte, sid uint32, payload []byte, err error) {
	if len(b) < 5 {
		return 0, 0, nil, errors.New("short frame")
	}
	return b[0], binary.BigEndian.Uint32(b[1:5]), b[5:], nil
}

// helloText is what an agent signs to prove it holds its key: the nonce the
// server just made, under a label nothing else signs.
func helloText(nonce string) []byte { return []byte("factor-rendezvous-v1\x00" + nonce) }

// SignHello answers a challenge.
func SignHello(id Signer, nonce string) string {
	return base64.StdEncoding.EncodeToString(id.Sign(helloText(nonce)))
}

// VerifyHello checks an agent's answer to a challenge.
func VerifyHello(key, nonce, sig string) (ed25519.PublicKey, error) {
	pub, err := DecodeKey(key)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(pub, helloText(nonce), raw) {
		return nil, errors.New("the signature does not match the key")
	}
	return pub, nil
}

// workBits counts the leading zero bits of the work hash.
func workBits(nonce, subject, work string) int {
	sum := sha256.Sum256([]byte(nonce + "\x00" + subject + "\x00" + work))
	n := 0
	for _, b := range sum {
		if b == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(b)
	}
	return n
}

// MaxWorkBits is the most proof of work an agent will pay: seconds on an old
// CPU. A rendezvous asking for more is refused rather than obeyed for hours.
const MaxWorkBits = 24

// SolveWork finds the proof of work a server asks before it registers a key
// or opens a mailbox: a counter whose hash with the nonce and the subject
// starts with the given number of zero bits.
func SolveWork(nonce, subject string, want int) (string, error) {
	if want > MaxWorkBits {
		return "", fmt.Errorf("the rendezvous asks for %d bits of proof of work, more than the %d this agent pays", want, MaxWorkBits)
	}
	for i := 0; ; i++ {
		w := strconv.Itoa(i)
		if workBits(nonce, subject, w) >= want {
			return w, nil
		}
	}
}

// CheckWork verifies it.
func CheckWork(nonce, subject, work string, want int) bool {
	return want <= 0 || (len(work) < 20 && workBits(nonce, subject, work) >= want)
}
