package swarm

import (
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIdentityPersistsPrivately(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "swarm")
	a, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || len(a.ID) != 32 || strings.ToLower(a.ID) != a.ID {
		t.Errorf("ids %q %q", a.ID, b.ID)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, identityFile))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("key file mode %v (%v)", info.Mode(), err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, identityFile), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(dir); err == nil {
		t.Error("a corrupt key loaded")
	}
	pub, err := DecodeKey(EncodeKey(a.Public))
	if err != nil || IDOf(pub) != a.ID {
		t.Error("key round trip")
	}
	if _, err := DecodeKey("bm9wZQ=="); err == nil {
		t.Error("a short key decoded")
	}
}

// handshake runs one mutual-TLS handshake over a loopback socket.
func handshake(server *tls.Config, client *tls.Config) (peer string, serverErr, clientErr error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err, err
	}
	defer ln.Close()
	done := make(chan error, 1)
	var seen string
	go func() {
		sc, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		conn := tls.Server(sc, server)
		err = conn.Handshake()
		if err == nil {
			seen, _ = PeerID(conn.ConnectionState())
		}
		done <- err
		_ = conn.Close()
	}()
	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return "", err, err
	}
	conn := tls.Client(cc, client)
	clientErr = conn.Handshake()
	if clientErr == nil {
		_, _ = conn.Read(make([]byte, 1))
	}
	_ = conn.Close()
	serverErr = <-done
	return seen, serverErr, clientErr
}

func TestPinnedMutualTLS(t *testing.T) {
	server, _ := NewIdentity()
	client, _ := NewIdentity()
	stranger, _ := NewIdentity()
	members := func(id string) bool { return id == client.ID }

	peer, serr, cerr := handshake(server.ServerTLS(members), client.ClientTLS(server.ID))
	if serr != nil || cerr != nil || peer != client.ID {
		t.Fatalf("member handshake: server %v client %v peer %q", serr, cerr, peer)
	}
	if _, serr, _ := handshake(server.ServerTLS(members), stranger.ClientTLS(server.ID)); serr == nil {
		t.Error("the server accepted an agent the roster does not list")
	}
	if _, _, cerr := handshake(server.ServerTLS(members), client.ClientTLS(stranger.ID)); cerr == nil {
		t.Error("the client accepted an agent other than the one it dialled")
	}
	if _, ok := PeerID(tls.ConnectionState{}); ok {
		t.Error("a connection with no certificate named a peer")
	}
	if _, err := peerKey(nil); err == nil {
		t.Error("no certificate")
	}
	if _, err := peerKey([][]byte{[]byte("junk")}); err == nil {
		t.Error("junk certificate")
	}
}
