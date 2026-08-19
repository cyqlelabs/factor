// Package proxy routes Factor's own HTTP through a proxy, so a debugging or
// auditing session can watch every provider call go past — prompts, tool
// definitions, replies and token counts, as they cross the wire.
//
// Any proxy will do: an intercepting one (mitmproxy, Burp, ZAP, Charles), a
// corporate egress proxy, a SOCKS5 listener. It works through the standard
// proxy environment variables rather than by pinning a transport, for two
// reasons: Go's own ProxyFromEnvironment already leaves loopback alone, so
// the memory sidecar and the speech server keep talking directly, and every
// child process Factor spawns — smrti, the voice shell — inherits the
// setting and shows up in the same capture.
package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// proxyEnv is what Go, curl, Python and most everything else read to find a
// proxy. Both cases are written because both are read in the wild.
var proxyEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"}

// caEnv is what the Python sidecars read to find a certificate authority.
// This process does not use them — it builds its own pool — so they exist
// purely so a spawned child trusts what this one trusts.
var caEnv = []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE"}

// wellKnownCAs are the paths intercepting proxies conventionally write their
// certificate authority to. Nothing but a convenience: --proxy-ca names one
// directly, and a proxy whose CA is already in the system store needs
// neither.
func wellKnownCAs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".mitmproxy", "mitmproxy-ca-cert.pem"),
		filepath.Join(home, ".BurpSuite", "burp-ca.pem"),
	}
}

// Use routes this process (and its children) through the proxy at raw. The
// returned line says what it did, for the caller to print.
//
// caPath names a certificate authority to trust on top of the system roots,
// which any TLS-intercepting proxy needs; empty means "look in the usual
// places". Trusting an extra CA is exactly as broad as it sounds, which is
// why it happens only when a proxy was explicitly asked for, and why the
// caller is told which certificate was trusted.
func Use(raw, caPath string) (string, error) {
	target, err := normalize(raw)
	if err != nil {
		return "", err
	}

	// The system roots are read before the CA environment below is set, so
	// this process keeps the real trust store and only its children inherit
	// the override.
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	trusted, err := trust(roots, caPath)
	if err != nil {
		return "", err
	}

	if err := install(roots, trusted != ""); err != nil {
		return "", err
	}
	for _, key := range proxyEnv {
		if err := os.Setenv(key, target); err != nil {
			return "", err
		}
	}
	if trusted != "" {
		for _, key := range caEnv {
			if err := os.Setenv(key, trusted); err != nil {
				return "", err
			}
		}
	}

	line := "routing HTTP through " + target
	if trusted != "" {
		line += ", trusting " + trusted
	}
	line += "; loopback and the browser are left alone"
	if hint := verify(target); hint != "" {
		line += "\n" + hint
	}
	return line, nil
}

// trust adds the named authority to roots, or the first well-known one that
// exists when none was named. It returns the path it used, empty when there
// was nothing to add — which is the ordinary case for a proxy that does not
// intercept TLS, and for one whose CA the system already trusts.
func trust(roots *x509.CertPool, caPath string) (string, error) {
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return "", fmt.Errorf("reading the proxy's certificate authority: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return "", fmt.Errorf("%s holds no certificate that can be trusted", caPath)
		}
		return caPath, nil
	}
	for _, path := range wellKnownCAs() {
		pem, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if roots.AppendCertsFromPEM(pem) {
			return path, nil
		}
	}
	return "", nil
}

// install replaces the default HTTP transport with a copy that trusts the
// extended pool. It clones rather than building one, so the connection
// pooling, timeouts and HTTP/2 settings the default carries are kept.
func install(roots *x509.CertPool, withRoots bool) error {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return errors.New("the default HTTP transport has been replaced; cannot route it through a proxy")
	}
	clone := transport.Clone()
	if withRoots {
		withRootCAs(clone, roots)
	}
	http.DefaultTransport = clone
	return nil
}

// withRootCAs points a transport's certificate verification at pool. A
// transport carrying no TLS settings of its own gets one, since that is the
// shape http.DefaultTransport is declared in.
func withRootCAs(t *http.Transport, pool *x509.CertPool) {
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	t.TLSClientConfig.RootCAs = pool
}

// probeURL is what verify asks for. It exists to be fetched and says nothing
// about the machine asking.
var probeURL = "https://example.com"

// verify makes one request through the proxy, so the two failures that can
// only mean one thing are reported here — where the person who typed -p is
// standing — rather than as a cryptic error inside a turn an hour later.
//
// Everything else passes in silence: a timeout, a refused host or a DNS
// failure could be the network, the probe host, or a proxy that blocks this
// one URL, and crying wolf about a proxy that works is worse than saying
// nothing.
func verify(target string) string {
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Get(probeURL)
	if err == nil {
		_ = resp.Body.Close()
		return ""
	}
	return hintFor(target, err)
}

// hintFor turns a probe failure into the sentence worth showing, or "" for
// the failures that could mean anything.
func hintFor(target string, err error) string {
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		issuer := "an authority this process does not know"
		if unknown.Cert != nil && unknown.Cert.Issuer.CommonName != "" {
			issuer = unknown.Cert.Issuer.CommonName
		}
		return fmt.Sprintf("warning: the proxy signs with %q, which is not trusted here — "+
			"export that proxy's CA certificate and pass it with --proxy-ca, or every HTTPS call will fail", issuer)
	}
	// Go marks a proxy it could not reach at all, as opposed to a site it
	// could not reach through the proxy.
	if strings.Contains(err.Error(), "proxyconnect") {
		return fmt.Sprintf("warning: nothing answered at %s — every HTTPS call will fail until it does", target)
	}
	return ""
}

// normalize accepts what a person types: a bare host:port, or a full URL.
func normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("no proxy address given")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("proxy address %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("proxy address %q names no host", raw)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("proxy scheme %q is not one of http, https, socks5", u.Scheme)
	}
	return u.String(), nil
}
