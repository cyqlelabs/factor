package swarm

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The direct path. Two members on one network talk over TCP with the same
// pinned TLS, and the relay stays the fallback. A first call never waits for
// it: the relay carries the call while a probe looks for the direct route
// alongside, and the requests after it move over once one answers.

// lanPort is where a member listens on its network when the direct path is
// on; a taken port falls back to any free one, which the record names.
var lanPort = 8741

// lanEvery is how often members without a direct path are looked at again.
var lanEvery = 5 * time.Minute

var errNoDirect = errors.New("no direct path to that member")

// addrRecord is where a member says it can be reached directly, signed by
// its key so the record means the same wherever it is kept.
type addrRecord struct {
	ID    string   `json:"id"`
	Addrs []string `json:"addrs"`
	At    int64    `json:"at"`
	Sig   string   `json:"sig,omitempty"`
}

func (r addrRecord) signed() []byte {
	r.Sig = ""
	data, _ := json.Marshal(r)
	return append([]byte("factor-swarm-addrs-v1\x00"), data...)
}

type lanPath struct {
	ln    net.Listener
	port  int
	addrs func(port int) []string

	mu   sync.Mutex
	good map[string]string
}

// startLAN listens on the local network and teaches the tunnel to dial
// members there.
func (s *Service) startLAN(ctx context.Context, tunnel *Tunnel) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(lanPort))
	if err != nil {
		if ln, err = net.Listen("tcp", ":0"); err != nil {
			slog.Warn("swarm: no direct path, the local network cannot be listened on", "error", err)
			return
		}
	}
	addrs := s.lanAddrs
	if addrs == nil {
		addrs = localAddrs
	}
	lan := &lanPath{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, addrs: addrs, good: map[string]string{}}
	s.netMu.Lock()
	s.lan = lan
	s.netMu.Unlock()
	tunnel.SetDirect(s.dialDirect)
	go func() { _ = tunnel.Serve(ln) }()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer ln.Close()
		ticker := time.NewTicker(lanEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.discoverAll()
			}
		}
	}()
}

func (s *Service) lanState() *lanPath {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	return s.lan
}

// dialDirect reaches a member over an address that answered a probe, and
// gives up at once otherwise: the relay is right there.
func (s *Service) dialDirect(ctx context.Context, peer string) (net.Conn, error) {
	lan := s.lanState()
	if lan == nil {
		return nil, errNoDirect
	}
	lan.mu.Lock()
	addr := lan.good[peer]
	lan.mu.Unlock()
	if addr == "" {
		return nil, errNoDirect
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		lan.mu.Lock()
		delete(lan.good, peer)
		lan.mu.Unlock()
		return nil, err
	}
	return conn, nil
}

// serveAddrs tells a member where this agent can be reached directly.
func (s *Service) serveAddrs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.peerOf(r); !ok {
		http.Error(w, "unknown agent", http.StatusUnauthorized)
		return
	}
	rec := addrRecord{ID: s.id.ID, At: s.now().Unix()}
	if lan := s.lanState(); lan != nil {
		rec.Addrs = lan.addrs(lan.port)
	}
	rec.Sig = base64.StdEncoding.EncodeToString(s.id.Sign(rec.signed()))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

// discoverAll looks for a direct path to every member that has none.
func (s *Service) discoverAll() {
	lan := s.lanState()
	if lan == nil {
		return
	}
	for _, m := range s.book.Read().Members(s.id.ID) {
		lan.mu.Lock()
		known := lan.good[m.ID] != ""
		lan.mu.Unlock()
		if known {
			continue
		}
		s.wg.Add(1)
		go func(peer string) {
			defer s.wg.Done()
			s.discover(peer)
		}(m.ID)
	}
}

// discover asks a member where it can be reached directly, checks the
// answer's signature against the key the roster lists for it, and probes each
// address with the pinned handshake. The first that answers is kept, and
// idle connections are closed so the next request dials it.
func (s *Service) discover(peer string) {
	lan := s.lanState()
	_, _, tunnel := s.net()
	if lan == nil || tunnel == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.bg, 15*time.Second)
	defer cancel()
	rec, err := s.fetchAddrs(ctx, tunnel, peer)
	if err != nil {
		slog.Debug("swarm: no address record", "peer", peer, "error", err)
		return
	}
	_ = s.book.Update(func(st *State) error {
		st.peer(peer).Addrs = slices.Clone(rec.Addrs)
		return nil
	})
	for _, addr := range rec.Addrs {
		if s.probe(ctx, peer, addr) {
			lan.mu.Lock()
			lan.good[peer] = addr
			lan.mu.Unlock()
			tunnel.closeIdle()
			slog.Info("swarm: direct path found", "peer", peer, "addr", addr)
			return
		}
	}
}

func (s *Service) fetchAddrs(ctx context.Context, tunnel *Tunnel, peer string) (addrRecord, error) {
	hc, base, err := tunnel.HTTPClient(peer)
	if err != nil {
		return addrRecord{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, join(base, "swarm/v1/addrs"), nil)
	if err != nil {
		return addrRecord{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return addrRecord{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return addrRecord{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var rec addrRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return addrRecord{}, err
	}
	m, ok := s.book.Read().Roster.Member(peer)
	if !ok || rec.ID != peer {
		return addrRecord{}, errors.New("the record is for another agent")
	}
	pub, err := DecodeKey(m.Key)
	sig, serr := base64.StdEncoding.DecodeString(rec.Sig)
	if err != nil || serr != nil || !ed25519.Verify(pub, rec.signed(), sig) {
		return addrRecord{}, errors.New("the record's signature does not match the member's key")
	}
	return rec, nil
}

// probe tries one address with the full pinned handshake: an address that
// answers but holds another key is not a path to this member.
func (s *Service) probe(ctx context.Context, peer, addr string) bool {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var d net.Dialer
	raw, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return false
	}
	conn := tls.Client(raw, s.id.ClientTLS(peer))
	ok := conn.HandshakeContext(dctx) == nil
	_ = conn.Close()
	return ok
}

// directCount is how many members are reached directly right now.
func (s *Service) directCount() int {
	lan := s.lanState()
	if lan == nil {
		return 0
	}
	lan.mu.Lock()
	defer lan.mu.Unlock()
	return len(lan.good)
}

// localAddrs lists this machine's addresses on private networks: the only
// ones a member on the same network could reach, and the only ones worth
// telling it about.
func localAddrs(port int) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || !ipnet.IP.IsPrivate() {
				continue
			}
			out = append(out, net.JoinHostPort(ipnet.IP.String(), strconv.Itoa(port)))
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		// IPv4 first: a home network is far likelier to route it.
		return strings.Compare(boolKey(strings.Contains(a, "[")), boolKey(strings.Contains(b, "[")))
	})
	return out
}

func boolKey(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
