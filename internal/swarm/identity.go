// Package swarm joins Factor agents on any machines into one swarm that hands
// work around over A2A (docs/swarm.md): who belongs, what each member may do
// here, the tasks this agent sends, and the turns other agents start.
package swarm

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Identity is this agent's Ed25519 key. The key is the agent: its id is a
// hash of the public half, its certificate is self-signed, and the others pin
// the key when they pair, so no certificate authority is involved anywhere.
type Identity struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
	ID      string
	cert    tls.Certificate
}

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

const identityFile = "identity.pem"

// LoadIdentity reads the key in dir, making one on first use. The file is
// readable by the owner alone: it is what every other member trusts.
func LoadIdentity(dir string) (*Identity, error) {
	path := filepath.Join(dir, identityFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s is not a PEM key", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an Ed25519 key", path)
	}
	return newIdentity(priv)
}

func newIdentity(priv ed25519.PrivateKey) (*Identity, error) {
	pub := priv.Public().(ed25519.PublicKey)
	id := &Identity{Private: priv, Public: pub, ID: IDOf(pub)}
	cert, err := selfSigned(id)
	if err != nil {
		return nil, err
	}
	id.cert = cert
	return id, nil
}

// NewIdentity makes a key that lives only in memory, for tests and tools that
// need a throwaway agent.
func NewIdentity() (*Identity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return newIdentity(priv)
}

// selfSigned makes the certificate the key is presented in. It is valid from
// 1970 to 2099 because the pin carries the trust, not the dates: a machine
// whose clock has drifted years must still be able to shake hands.
func selfSigned(id *Identity) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: id.ID},
		DNSNames:     []string{id.ID + HostSuffix},
		NotBefore:    time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, id.Public, id.Private)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: id.Private}, nil
}

// Sign signs msg with the agent's key.
func (id *Identity) Sign(msg []byte) []byte { return ed25519.Sign(id.Private, msg) }

// HostSuffix makes an agent id a host name: the A2A client dials
// https://<agent-id>.swarm/, and the tunnel resolves the name to the agent.
const HostSuffix = ".swarm"

// peerKey reads the Ed25519 key a TLS peer presented.
func peerKey(rawCerts [][]byte) (ed25519.PublicKey, error) {
	if len(rawCerts) == 0 {
		return nil, errors.New("the peer presented no certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return nil, err
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the peer's certificate is not an Ed25519 key")
	}
	// The certificate must be signed by the key it carries, or it says
	// nothing about who holds that key.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		return nil, fmt.Errorf("the peer's certificate is not self-signed: %w", err)
	}
	return pub, nil
}

// PeerID is the agent id behind a finished handshake.
func PeerID(cs tls.ConnectionState) (string, bool) {
	if len(cs.PeerCertificates) == 0 {
		return "", false
	}
	pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", false
	}
	return IDOf(pub), true
}

// ServerTLS accepts a connection only from an agent allowed names. Both sides
// present their key: this is mutual TLS 1.3 with each side pinned.
func (id *Identity) ServerTLS(allowed func(peer string) bool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.cert},
		ClientAuth:   tls.RequireAnyClientCert,
		NextProtos:   []string{"h2"},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			pub, err := peerKey(raw)
			if err != nil {
				return err
			}
			if peer := IDOf(pub); !allowed(peer) {
				return fmt.Errorf("agent %s is not a member of this swarm", peer)
			}
			return nil
		},
	}
}

// ClientTLS connects only to the agent want, whatever relay or address the
// bytes travel through.
func (id *Identity) ClientTLS(want string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{id.cert},
		InsecureSkipVerify: true, // the pin below is the verification
		NextProtos:         []string{"h2"},
		ServerName:         want + HostSuffix,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			pub, err := peerKey(raw)
			if err != nil {
				return err
			}
			if got := IDOf(pub); got != want {
				return fmt.Errorf("expected agent %s, reached %s", want, got)
			}
			return nil
		},
	}
}
