package swarm

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/psanford/wormhole-william/rendezvous"
	"salsa.debian.org/vasudev/gospake2"

	"github.com/cyqlelabs/factor/internal/swarm/wire"
)

// pairAppID namespaces Factor's mailboxes on a rendezvous.
const pairAppID = "fctr.app/swarm/pair/v1"

// CodeTTL is how long a pairing code works.
const CodeTTL = 10 * time.Minute

// attachWait bounds claiming a code's mailbox.
var attachWait = 5 * time.Second

// errWrongCode is a pairing whose two sides did not hold the same code.
var errWrongCode = errors.New("the code did not match; it is used up now, so make a new one")

// envelope is what each side of a pairing tells the other, encrypted under
// the key SPAKE2 agreed from the code. Proof is the agent's signature over
// this pairing's transcript: it shows the sender holds the key it names,
// bound to this exchange and no other.
type envelope struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Proof string `json:"proof"`
	// The sponsor, a member already, also says where the swarm is.
	Swarm      string `json:"swarm,omitempty"`
	Rendezvous string `json:"rendezvous,omitempty"`
}

// pairing is one side of a pairing in progress, in a mailbox on the
// rendezvous.
type pairing struct {
	rc    *rendezvous.Client
	stop  context.CancelFunc
	side  string
	other string
	code  string
	msgs  <-chan rendezvous.MailboxEvent
}

// httpBase turns a rendezvous address into the HTTP one its work endpoint
// answers on.
func httpBase(base string) string {
	switch {
	case strings.HasPrefix(base, "wss://"):
		return "https://" + strings.TrimPrefix(base, "wss://")
	case strings.HasPrefix(base, "ws://"):
		return "http://" + strings.TrimPrefix(base, "ws://")
	}
	return base
}

// openMailbox connects to the mailbox side of the rendezvous, paying the
// proof of work it asks of every mailbox. The client reads its socket for as
// long as the context it connected with lives, so the connection gets the
// code's lifetime rather than the caller's — an invite returns long before
// the other machine answers — and stop ends it.
func openMailbox(ctx context.Context, base string) (rc *rendezvous.Client, side string, stop context.CancelFunc, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpBase(base)+"/v1/work", nil)
	if err != nil {
		return nil, "", nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", nil, fmt.Errorf("the rendezvous is not reachable: %w", err)
	}
	defer resp.Body.Close()
	var challenge struct {
		Nonce string `json:"nonce"`
		Bits  int    `json:"bits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&challenge); err != nil {
		return nil, "", nil, fmt.Errorf("the rendezvous did not answer as one: %w", err)
	}
	work, err := wire.SolveWork(challenge.Nonce, "mailbox", challenge.Bits)
	if err != nil {
		return nil, "", nil, err
	}
	raw := make([]byte, 5)
	_, _ = rand.Read(raw)
	side = hex.EncodeToString(raw)
	u := strings.TrimRight(base, "/") + "/v1/mailbox?work=" + url.QueryEscape(challenge.Nonce+"~"+work)
	rc = rendezvous.NewClient(u, side, pairAppID)
	life, stop := context.WithTimeout(context.Background(), CodeTTL+time.Minute)
	connected := make(chan error, 1)
	go func() {
		_, err := rc.Connect(life)
		connected <- err
	}()
	select {
	case err = <-connected:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		stop()
		return nil, "", nil, fmt.Errorf("could not open a pairing mailbox: %w", err)
	}
	return rc, side, stop, nil
}

// newPassword is the secret half of a code: eight random digits, one guess
// in a hundred million.
func newPassword() string {
	var b strings.Builder
	for range passwordDigits {
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		b.WriteByte(byte('0' + n.Int64()))
	}
	return b.String()
}

// makeCode allocates a mailbox and makes the code that finds it.
func makeCode(ctx context.Context, base string) (*pairing, error) {
	rc, side, stop, err := openMailbox(ctx, base)
	if err != nil {
		return nil, err
	}
	nameplate, err := rc.CreateMailbox(ctx)
	if err != nil {
		stop()
		return nil, fmt.Errorf("could not allocate a pairing code: %w", err)
	}
	p := &pairing{rc: rc, stop: stop, side: side, code: nameplate + "-" + newPassword()}
	p.msgs = rc.MsgChan(context.Background())
	return p, nil
}

// takeCode opens the mailbox a code names.
func takeCode(ctx context.Context, base, code string) (*pairing, error) {
	nameplate, password, err := codeParts(code)
	if err != nil {
		return nil, err
	}
	rc, side, stop, err := openMailbox(ctx, base)
	if err != nil {
		return nil, err
	}
	// The client never reports a refused claim, it only waits for the reply
	// that will not come; a code the rendezvous holds answers in a moment.
	actx, cancel := context.WithTimeout(ctx, attachWait)
	defer cancel()
	if err := rc.AttachMailbox(actx, nameplate); err != nil {
		stop()
		return nil, fmt.Errorf("no pairing is waiting under that code: %w", err)
	}
	p := &pairing{rc: rc, stop: stop, side: side, code: nameplate + "-" + password}
	p.msgs = rc.MsgChan(context.Background())
	return p, nil
}

// next waits for the other side's message in a phase.
func (p *pairing) next(ctx context.Context, phase string) (string, error) {
	for {
		select {
		case ev, ok := <-p.msgs:
			if !ok {
				return "", errors.New("the pairing mailbox closed")
			}
			if ev.Error != nil {
				return "", ev.Error
			}
			if ev.Side != p.side && ev.Phase == phase {
				p.other = ev.Side
				return ev.Body, nil
			}
		case <-ctx.Done():
			return "", fmt.Errorf("the other machine did not answer in time: %w", ctx.Err())
		}
	}
}

// sealer derives the key one side encrypts with.
func sealer(key []byte, side string) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, key, nil, "factor-pair-v1/"+side, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// transcript is what both proofs sign: a hash of the agreed key, which only
// the two sides of this pairing know.
func transcript(key []byte) []byte {
	sum := sha256.Sum256(append([]byte("factor-pair-v1/transcript\x00"), key...))
	return sum[:]
}

// exchange runs SPAKE2 over the mailbox and swaps envelopes. A wrong code
// fails on both sides and burns the mailbox, so it gives no second guess.
func (p *pairing) exchange(ctx context.Context, id *Identity, mine envelope) (envelope, error) {
	ctx, cancel := context.WithTimeout(ctx, CodeTTL)
	defer cancel()
	spake := gospake2.SPAKE2Symmetric(gospake2.NewPassword(p.code), gospake2.NewIdentityS(pairAppID))
	if err := p.rc.AddMessage(ctx, "pake", hex.EncodeToString(spake.Start())); err != nil {
		return envelope{}, err
	}
	body, err := p.next(ctx, "pake")
	if err != nil {
		return envelope{}, err
	}
	other, err := hex.DecodeString(body)
	if err != nil {
		return envelope{}, p.fail(errors.New("the other machine sent a malformed key exchange"))
	}
	key, err := spake.Finish(other)
	if err != nil {
		return envelope{}, p.fail(errWrongCode)
	}
	mine.Key = EncodeKey(id.Public)
	mine.Proof = base64.StdEncoding.EncodeToString(id.Sign(transcript(key)))
	plain, _ := json.Marshal(mine)
	out, err := sealer(key, p.side)
	if err != nil {
		return envelope{}, err
	}
	nonce := make([]byte, out.NonceSize())
	_, _ = rand.Read(nonce)
	if err := p.rc.AddMessage(ctx, "envelope", hex.EncodeToString(out.Seal(nonce, nonce, plain, nil))); err != nil {
		return envelope{}, err
	}
	body, err = p.next(ctx, "envelope")
	if err != nil {
		return envelope{}, err
	}
	sealed, err := hex.DecodeString(body)
	in, serr := sealer(key, p.peerSide())
	if err != nil || serr != nil || len(sealed) < in.NonceSize() {
		return envelope{}, p.fail(errWrongCode)
	}
	opened, err := in.Open(nil, sealed[:in.NonceSize()], sealed[in.NonceSize():], nil)
	if err != nil {
		return envelope{}, p.fail(errWrongCode)
	}
	var theirs envelope
	if err := json.Unmarshal(opened, &theirs); err != nil {
		return envelope{}, p.fail(errors.New("the other machine sent a malformed envelope"))
	}
	pub, err := DecodeKey(theirs.Key)
	sig, serr := base64.StdEncoding.DecodeString(theirs.Proof)
	if err != nil || serr != nil || !ed25519.Verify(pub, transcript(key), sig) {
		return envelope{}, p.fail(errors.New("the other machine could not prove it holds the key it sent"))
	}
	return theirs, nil
}

// peerSide is the other side's id, read off its key exchange: a mailbox
// carries exactly two sides.
func (p *pairing) peerSide() string { return p.other }

// fail closes the mailbox the way that burns it, and returns err.
func (p *pairing) fail(err error) error {
	p.close(rendezvous.Scary)
	return err
}

// done releases the mailbox after a pairing that worked.
func (p *pairing) done() { p.close(rendezvous.Happy) }

func (p *pairing) close(mood rendezvous.Mood) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.rc.Close(ctx, mood)
	p.stop()
}
