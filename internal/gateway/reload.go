package gateway

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// ControlAddr is the address a terminal reaches the running gateway on. A
// daemon bound to every interface is still asked over loopback, which is the
// only address its reload endpoint answers.
func ControlAddr(host string, port int) string {
	if host == "" || host == "0.0.0.0" || host == "::" || strings.Contains(host, "*") {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// reloadHandler is where a terminal asks the daemon to reload on a platform
// with no signal to carry it. `factor upgrade` installs the new binary and the
// running gateway keeps executing the old one until something tells it — on
// unix a SIGHUP, and here for Windows, which has neither that nor any other
// way to reach a process it did not start.
//
// Loopback only: the health port is a status page the user may well have bound
// to something wider than localhost, and a restart anybody on the network can
// ask for is a different thing entirely.
func reloadHandler(reload func(string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r.RemoteAddr) {
			http.Error(w, "reload is asked from this machine only", http.StatusForbidden)
			return
		}
		reload("a reload asked over the control endpoint")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("reloading once the turn in flight is answered\n"))
	}
}

// fromLoopback reports whether a request came from this machine. An address
// that cannot be parsed is not one of ours.
func fromLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
