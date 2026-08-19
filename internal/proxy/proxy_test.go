package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keepTransport restores the process-wide transport these tests replace.
func keepTransport(t *testing.T) {
	t.Helper()
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
}

// clearProxyEnv detaches a test from whatever the developer's shell exports.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, key := range append(append([]string{}, proxyEnv...), caEnv...) {
		t.Setenv(key, "")
	}
}

// caPEM is a certificate authority, minted here so the test needs no fixture
// file and no network.
const caPEM = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----`

func writeCA(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNormalizeAcceptsWhatAPersonTypes(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8080":          "http://127.0.0.1:8080",
		" localhost:9090 ":        "http://localhost:9090",
		"http://proxy.corp:3128":  "http://proxy.corp:3128",
		"https://proxy.corp":      "https://proxy.corp",
		"socks5://127.0.0.1:9050": "socks5://127.0.0.1:9050",
	} {
		got, err := normalize(in)
		if err != nil || got != want {
			t.Errorf("normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", "ftp://proxy:21", "http://", "://nope"} {
		if got, err := normalize(bad); err == nil {
			t.Errorf("normalize(%q) = %q, want an error", bad, got)
		}
	}
}

func TestUseRoutesTheProcessAndItsChildren(t *testing.T) {
	keepTransport(t)
	clearProxyEnv(t)
	ca := writeCA(t, caPEM)

	line, err := Use("127.0.0.1:9", ca) // port 9 discards, so the probe stays quiet
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range proxyEnv {
		if got := os.Getenv(key); got != "http://127.0.0.1:9" {
			t.Errorf("%s = %q, want the proxy", key, got)
		}
	}
	// The sidecars read these, and only them: this process built its own pool.
	for _, key := range caEnv {
		if got := os.Getenv(key); got != ca {
			t.Errorf("%s = %q, want %q", key, got, ca)
		}
	}
	if !strings.Contains(line, "trusting "+ca) || !strings.Contains(line, "loopback") {
		t.Errorf("report = %q", line)
	}
}

// The default transport carries no TLS config at all, so the trust pool has
// to be created rather than cloned — getting that wrong silently drops the
// certificate while still reporting it as trusted.
func TestUseActuallyInstallsTheTrustPool(t *testing.T) {
	keepTransport(t)
	clearProxyEnv(t)
	if _, err := Use("127.0.0.1:9", writeCA(t, caPEM)); err != nil {
		t.Fatal(err)
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("the default transport is no longer an *http.Transport")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("the proxy's certificate authority was reported as trusted but never installed")
	}
	if n := len(transport.TLSClientConfig.RootCAs.Subjects()); n == 0 { //nolint:staticcheck // counting is the point
		t.Error("the trust pool is empty")
	}
}

func TestUseWithoutACertificateLeavesTrustAlone(t *testing.T) {
	keepTransport(t)
	clearProxyEnv(t)
	t.Setenv("HOME", t.TempDir()) // no well-known CA to find
	line, err := Use("proxy.corp:3128", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, "trusting") {
		t.Errorf("report = %q, want no certificate mentioned", line)
	}
	if transport := http.DefaultTransport.(*http.Transport); transport.TLSClientConfig != nil &&
		transport.TLSClientConfig.RootCAs != nil {
		t.Error("a proxy with no CA still redirected certificate verification")
	}
}

func TestWithRootCAsWorksOnATransportThatCarriesNoTLSSettings(t *testing.T) {
	pool := x509.NewCertPool()
	bare := &http.Transport{}
	withRootCAs(bare, pool)
	if bare.TLSClientConfig == nil || bare.TLSClientConfig.RootCAs != pool {
		t.Error("a bare transport did not receive the trust pool")
	}
	existing := &http.Transport{TLSClientConfig: &tls.Config{ServerName: "keep.me", MinVersion: tls.VersionTLS12}}
	withRootCAs(existing, pool)
	if existing.TLSClientConfig.ServerName != "keep.me" {
		t.Error("existing TLS settings were discarded")
	}
	if existing.TLSClientConfig.RootCAs != pool {
		t.Error("the trust pool did not reach a transport that already had settings")
	}
}

func TestUseFindsAWellKnownCertificate(t *testing.T) {
	keepTransport(t)
	clearProxyEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".mitmproxy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mitmproxy-ca-cert.pem")
	if err := os.WriteFile(path, []byte(caPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	line, err := Use("127.0.0.1:9", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, path) {
		t.Errorf("report = %q, want it to name the certificate it found", line)
	}
}

func TestUseRefusesACertificateItCannotRead(t *testing.T) {
	keepTransport(t)
	clearProxyEnv(t)
	if _, err := Use("127.0.0.1:9", filepath.Join(t.TempDir(), "absent.pem")); err == nil {
		t.Error("a missing certificate was accepted")
	}
	if _, err := Use("127.0.0.1:9", writeCA(t, "not a certificate")); err == nil {
		t.Error("a file with no certificate in it was accepted")
	}
	if _, err := Use("ftp://nope", ""); err == nil {
		t.Error("an unusable proxy scheme was accepted")
	}
}

func TestHintNamesOnlyTheFailuresThatCanMeanOneThing(t *testing.T) {
	// A rejection that carries no certificate must still be reported, not
	// crash on the name it does not have.
	if hint := hintFor("http://p:9", &url1{err: x509.UnknownAuthorityError{}}); !strings.Contains(hint, "not trusted here") {
		t.Errorf("an untrusted authority produced %q", hint)
	}
	if hint := hintFor("http://p:9", errors.New(`Get "https://x": proxyconnect tcp: connection refused`)); !strings.Contains(hint, "nothing answered") {
		t.Errorf("an unreachable proxy produced %q", hint)
	}
	for _, quiet := range []error{
		errors.New("context deadline exceeded"),
		errors.New("dial tcp: lookup example.com: no such host"),
	} {
		if hint := hintFor("http://p:9", quiet); hint != "" {
			t.Errorf("%v produced %q, want silence", quiet, hint)
		}
	}
}

// url1 wraps an error the way net/http does, so the classifier is tested
// through the same unwrapping it meets in production.
type url1 struct{ err error }

func (u *url1) Error() string { return "Get \"https://x\": " + u.err.Error() }
func (u *url1) Unwrap() error { return u.err }

func TestVerifyReportsAnInterceptingProxyItCannotTrust(t *testing.T) {
	keepTransport(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	original := probeURL
	probeURL = srv.URL
	defer func() { probeURL = original }()

	if hint := verify("http://p:9"); !strings.Contains(hint, "not trusted here") {
		t.Errorf("probing a server signed by an unknown authority produced %q", hint)
	}
	// The same server, trusted, is silence.
	http.DefaultTransport = srv.Client().Transport
	if hint := verify("http://p:9"); hint != "" {
		t.Errorf("a working proxy produced %q", hint)
	}
}
