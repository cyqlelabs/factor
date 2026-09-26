package memory

import (
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// processEnv reports a process's environment, read from the kernel on
// Linux. Elsewhere there is nothing to read it from, so the settings check
// stays quiet there rather than guessing. A var so the check can be tested
// against an engine whose environment is not really what it is.
var processEnv = envOf

func envOf(pid int) (map[string]string, bool) {
	if runtime.GOOS != "linux" {
		return nil, false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, false
	}
	return parseEnviron(strings.Split(string(data), "\x00")), true
}

func parseEnviron(entries []string) map[string]string {
	env := make(map[string]string, len(entries))
	for _, e := range entries {
		if k, v, ok := strings.Cut(e, "="); ok && k != "" {
			env[k] = v
		}
	}
	return env
}

// engineSettings keeps the variables that configure the engine — the ones
// buildEnv writes, which are the ones a newer Factor can add, drop or
// change — out of an environment that also holds the shell's own.
func engineSettings(env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		if strings.HasPrefix(k, "SMRTI_") || k == "ORT_DISABLE_TELEMETRY" || k == "MALLOC_ARENA_MAX" {
			out[k] = v
		}
	}
	return out
}

// settingsDrift names the settings that differ between what the engine was
// started with and what it would be started with now, sorted. Names only:
// the values hold the API key.
func settingsDrift(want, have map[string]string) []string {
	var changed []string
	for k, v := range want {
		if hv, ok := have[k]; !ok || hv != v {
			changed = append(changed, k)
		}
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed
}
