package memory

import (
	"errors"
	"reflect"
	"testing"
)

// The macOS and Windows lookups can only be exercised where their helper
// runs, which is never the machine this suite is developed on — so the
// parsing is split out and checked against output captured from each.

func TestParseLsof(t *testing.T) {
	// `lsof -t` prints one pid a line: the server and the workers it forked.
	if got := parseLsof("4821\n4822\n4823\n"); !reflect.DeepEqual(got, []int{4821, 4822, 4823}) {
		t.Errorf("pids = %v", got)
	}
	if got := parseLsof(""); got != nil {
		t.Errorf("empty output = %v, want nothing", got)
	}
	if got := parseLsof("not a pid\n"); got != nil {
		t.Errorf("garbage = %v, want nothing", got)
	}
}

func TestParseNetstat(t *testing.T) {
	// Captured from `netstat -ano -p tcp`: a header, the port listening, the
	// same port on IPv6, an established connection to it, a different port,
	// and a longer port that ends in the same digits.
	out := "\r\nActive Connections\r\n\r\n" +
		"  Proto  Local Address          Foreign Address        State           PID\r\n" +
		"  TCP    127.0.0.1:8420         0.0.0.0:0              LISTENING       6120\r\n" +
		"  TCP    [::1]:8420             [::]:0                 LISTENING       6120\r\n" +
		"  TCP    127.0.0.1:8420         127.0.0.1:51544        ESTABLISHED     9001\r\n" +
		"  TCP    127.0.0.1:84200        0.0.0.0:0              LISTENING       7777\r\n" +
		"  TCP    127.0.0.1:9999         0.0.0.0:0              LISTENING       4242\r\n"
	got := parseNetstat(out, 8420)
	if !reflect.DeepEqual(got, []int{6120, 6120}) {
		t.Errorf("pids = %v, want the two listening rows for 8420 only", got)
	}
	if got := parseNetstat(out, 9999); !reflect.DeepEqual(got, []int{4242}) {
		t.Errorf("other port = %v", got)
	}
	if got := parseNetstat(out, 1234); got != nil {
		t.Errorf("a port nothing holds = %v", got)
	}
	if got := parseNetstat("garbage\nshort line\n", 8420); got != nil {
		t.Errorf("garbage = %v", got)
	}
}

// A helper the machine does not have says nothing, rather than reporting a
// port as free — the caller would then spawn a second engine onto it.
func TestListenersSayNothingWhenTheHelperIsMissing(t *testing.T) {
	saved := helperOutput
	helperOutput = func(string, ...string) (string, error) { return "", errors.New("executable file not found") }
	t.Cleanup(func() { helperOutput = saved })
	if got := lsofListeners(8420); got != nil {
		t.Errorf("lsof = %v", got)
	}
	if got := netstatListeners(8420); got != nil {
		t.Errorf("netstat = %v", got)
	}
}

// A port nobody asked about is not a port to go looking for: the helpers
// would list every listener on the machine.
func TestListenerPidIgnoresANonPort(t *testing.T) {
	for _, port := range []int{0, -1} {
		if pid, ok := ListenerPid(port); ok || pid != 0 {
			t.Errorf("ListenerPid(%d) = %d, %v", port, pid, ok)
		}
	}
}
