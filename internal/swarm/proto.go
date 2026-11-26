package swarm

import (
	"crypto/ed25519"

	"github.com/cyqlelabs/factor/internal/swarm/wire"
)

// Control is one control message on the agent socket.
type Control = wire.Control

const (
	CtlChallenge = wire.CtlChallenge
	CtlHello     = wire.CtlHello
	CtlWelcome   = wire.CtlWelcome
	CtlCommit    = wire.CtlCommit
	CtlCommitted = wire.CtlCommitted
	CtlRoster    = wire.CtlRoster
	CtlFetch     = wire.CtlFetch
	CtlPresence  = wire.CtlPresence
	CtlError     = wire.CtlError

	FrameOpen   = wire.FrameOpen
	FrameData   = wire.FrameData
	FrameClose  = wire.FrameClose
	FrameCredit = wire.FrameCredit
	MaxFrame    = wire.MaxFrame
)

// SignHello answers a rendezvous challenge with this agent's key.
func (id *Identity) SignHello(nonce string) string { return wire.SignHello(id, nonce) }

// PublicKey makes Identity a wire.Signer.
func (id *Identity) PublicKey() ed25519.PublicKey { return id.Public }
