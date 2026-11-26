package wire

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"
)

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// IDOf is the agent id for a public key: 32 lowercase base32 characters, so
// one id is at once a session key, a file name and a host name label.
func IDOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return strings.ToLower(idEncoding.EncodeToString(sum[:20]))
}

// EncodeKey and DecodeKey write a public key the way the roster carries it.
func EncodeKey(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

func DecodeKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("not an Ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

// NewSwarmID makes the random id a new swarm is known by.
func NewSwarmID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return strings.ToLower(idEncoding.EncodeToString(b))
}
