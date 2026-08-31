package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// The engine as docker describes it: one container, publishing the memory port,
// carrying a handful of settings on top of what its image already provides.
const engineInspect = `[{
  "Name": "/smrti",
  "Image": "sha256:0ldimage",
  "Config": {
    "Image": "ghcr.io/cyqlelabs/smrti:0.9.0",
    "Env": ["PATH=/usr/local/bin", "SMRTI_SPACE=main", "SMRTI_PERSONALITY=maverick"],
    "Cmd": ["serve", "rest", "--host", "0.0.0.0", "--port", "8420"],
    "User": "",
    "Labels": {}
  },
  "HostConfig": {
    "Binds": ["/root/smrti_data:/data"],
    "NetworkMode": "default",
    "PortBindings": {"8420/tcp": [{"HostIp": "127.0.0.1", "HostPort": "8420"}]},
    "RestartPolicy": {"Name": "unless-stopped"}
  }
}]`

const engineImage = `{"Env":["PATH=/usr/local/bin"],"Cmd":["serve","rest","--host","0.0.0.0","--port","8420"],"User":"smrti"}`

// fakeDocker answers docker invocations from reply and records every call.
func fakeDocker(t *testing.T, reply func(args []string) (string, error)) *[]string {
	t.Helper()
	prevLook, prevCmd := dockerLook, dockerCmd
	calls := []string{}
	dockerLook = func() error { return nil }
	dockerCmd = func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return reply(args)
	}
	t.Cleanup(func() { dockerLook, dockerCmd = prevLook, prevCmd })
	return &calls
}

// engineDocker is the fake for a healthy, containerised engine. Every command
// starting with one of failOn fails, which is how the rollback paths are
// reached.
func engineDocker(t *testing.T, failOn ...string) *[]string {
	t.Helper()
	return fakeDocker(t, func(args []string) (string, error) {
		joined := strings.Join(args, " ")
		for _, prefix := range failOn {
			if strings.HasPrefix(joined, prefix) {
				return "", fmt.Errorf("docker %s: exit status 125", prefix)
			}
		}
		switch args[0] {
		case "ps":
			return "c0ffee\n", nil
		case "inspect":
			return engineInspect, nil
		case "image":
			return engineImage, nil
		default:
			return "", nil
		}
	})
}

// fakeRegistry publishes tags at registryBase.
func fakeRegistry(t *testing.T, tags ...string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"token":"anonymous"}`)
	})
	mux.HandleFunc("/v2/"+smrtiRepo+"/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anonymous" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := json.Marshal(map[string]any{"tags": tags})
		w.Write(body)
	})
	srv := httptest.NewServer(mux)
	prev := registryBase
	registryBase = srv.URL
	t.Cleanup(func() { registryBase = prev; srv.Close() })
}

// engineConfig points the health probe at a server that answers like smrti.
func engineConfig(t *testing.T, answering bool) config.MemoryConfig {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !answering {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"spaces":{},"space":"main"}`)
	}))
	t.Cleanup(srv.Close)
	return config.MemoryConfig{Mode: "external", URL: srv.URL, Host: "127.0.0.1", Port: 8420}
}

// quickPacing shrinks the waits so a test does not sit through a real restart.
func quickPacing(t *testing.T) {
	t.Helper()
	prevIdleWait, prevIdlePoll := smrtiIdleWait, smrtiIdlePoll
	prevHealthWait, prevHealthPoll := smrtiHealthWait, smrtiHealthPoll
	smrtiIdleWait, smrtiIdlePoll = 60*time.Millisecond, 10*time.Millisecond
	smrtiHealthWait, smrtiHealthPoll = 60*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		smrtiIdleWait, smrtiIdlePoll = prevIdleWait, prevIdlePoll
		smrtiHealthWait, smrtiHealthPoll = prevHealthWait, prevHealthPoll
	})
}

func TestSmrtiCheck(t *testing.T) {
	engineDocker(t)
	fakeRegistry(t, "0.9.0", "0.9.1", "0.9", "latest", "not-a-version")

	rel, err := NewSmrti(engineConfig(t, true), nil).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Running != "0.9.0" || rel.Version != "0.9.1" || rel.Container != "smrti" {
		t.Fatalf("release = %+v", rel)
	}
	if !strings.HasSuffix(rel.Image, "/"+smrtiRepo+":0.9.1") {
		t.Errorf("image = %q", rel.Image)
	}
	if !rel.Newer() {
		t.Error("0.9.1 should be newer than the running 0.9.0")
	}
}

func TestSmrtiCheckWithoutADockerContainerOrAnInstall(t *testing.T) {
	noInstall(t)

	// No docker at all.
	prev := dockerLook
	dockerLook = func() error { return fmt.Errorf("not found") }
	_, err := NewSmrti(config.MemoryConfig{Port: 8420}, nil).Check(context.Background())
	dockerLook = prev
	if !errors.Is(err, ErrNotManaged) || !strings.Contains(err.Error(), "docker is not installed") {
		t.Fatalf("error = %v", err)
	}

	// Docker, but nothing publishing the memory port.
	fakeDocker(t, func(args []string) (string, error) {
		if args[0] == "ps" {
			return "c0ffee\n", nil
		}
		return strings.Replace(engineInspect, `"HostPort": "8420"`, `"HostPort": "9999"`, 1), nil
	})
	if _, err := NewSmrti(config.MemoryConfig{Port: 8420}, nil).Check(context.Background()); !errors.Is(err, ErrNotManaged) ||
		!strings.Contains(err.Error(), "publishes port 8420") {
		t.Fatalf("error = %v", err)
	}

	// Docker, nothing running at all.
	fakeDocker(t, func([]string) (string, error) { return "\n", nil })
	if _, err := NewSmrti(config.MemoryConfig{Port: 8420}, nil).Check(context.Background()); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("a machine running no smrti at all has none to upgrade: %v", err)
	}

	// A docker that answers with an error is not a machine without an engine:
	// the container it holds may well be the engine, and upgrading a package
	// instead of it would change nothing.
	fakeDocker(t, func(args []string) (string, error) {
		if args[0] == "ps" {
			return "", fmt.Errorf("docker ps: permission denied")
		}
		return "", nil
	})
	if _, err := NewSmrti(config.MemoryConfig{Port: 8420}, nil).Check(context.Background()); err == nil ||
		errors.Is(err, ErrNotManaged) {
		t.Fatalf("error = %v", err)
	}
}

func TestRunArgsCopiesTheContainerButNotTheImage(t *testing.T) {
	engineDocker(t)
	var found []container
	if err := json.Unmarshal([]byte(engineInspect), &found); err != nil {
		t.Fatal(err)
	}
	c := found[0]
	c.Config.User = "999:999"
	c.Config.Labels = map[string]string{"role": "memory"}
	c.HostConfig.NetworkMode = "factornet"

	args, err := runArgs(context.Background(), c, "ghcr.io/cyqlelabs/smrti:0.9.1")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(args, " ")
	for _, want := range []string{
		"-d --name smrti",
		"--restart unless-stopped",
		"--network factornet",
		"--user 999:999",
		"-v /root/smrti_data:/data",
		"-p 127.0.0.1:8420:8420/tcp",
		"--label role=memory",
		"-e SMRTI_SPACE=main",
		"-e SMRTI_PERSONALITY=maverick",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("run args %q missing %q", line, want)
		}
	}
	// PATH came from the image, so the new image decides it.
	if strings.Contains(line, "PATH=") {
		t.Errorf("image environment was copied onto the new container: %q", line)
	}
	// The command is the image's own default, so it must not be pinned: 0.9
	// moved `smrti` into the entrypoint, and a copied command would double it.
	if !strings.HasSuffix(line, "ghcr.io/cyqlelabs/smrti:0.9.1") {
		t.Errorf("the image's default command was pinned onto the new container: %q", line)
	}

	// A command the operator chose, however, is theirs and survives.
	c.Config.Cmd = []string{"serve", "rest", "--host", "0.0.0.0", "--port", "9000"}
	args, err = runArgs(context.Background(), c, "ghcr.io/cyqlelabs/smrti:0.9.1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.Join(args, " "), "--port 9000") {
		t.Errorf("run args = %v", args)
	}
}

func TestSmrtiApply(t *testing.T) {
	quickPacing(t)
	calls := engineDocker(t)
	fakeRegistry(t, "0.9.1")

	// Busy for the first two looks, then quiet: the swap must wait for it.
	looks := 0
	idle := func() bool { looks++; return looks > 2 }

	s := NewSmrti(engineConfig(t, true), idle)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	if _, err := s.Apply(context.Background(), rel, func(f string, a ...any) {
		steps = append(steps, fmt.Sprintf(f, a...))
	}); err != nil {
		t.Fatal(err)
	}

	line := strings.Join(*calls, " | ")
	for _, want := range []string{
		"pull " + rel.Image,
		"stop --time " + smrtiStopTimeout + " smrti",
		"rename smrti smrti-0.9.0",
		"run -d --name smrti",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("docker calls %q missing %q", line, want)
		}
	}
	if strings.Index(line, "pull") > strings.Index(line, "stop --time") {
		t.Error("the image must be on disk before the engine goes down")
	}
	if looks <= 2 {
		t.Errorf("the swap did not wait for the graph to go quiet (%d looks)", looks)
	}
	if !strings.Contains(strings.Join(steps, " "), "quiet") {
		t.Errorf("progress = %v", steps)
	}
}

func TestSmrtiApplyKeepsOneRollbackAndNoMore(t *testing.T) {
	quickPacing(t)
	calls := fakeDocker(t, func(args []string) (string, error) {
		switch {
		case args[0] == "ps" && len(args) > 1 && args[1] == "-a":
			// Two older generations, plus a container that only shares the
			// prefix and has nothing to do with this.
			return "smrti\nsmrti-0.8.2\nsmrti-previous\nsmrti-probe\n", nil
		case args[0] == "ps":
			return "c0ffee\n", nil
		case args[0] == "inspect":
			return engineInspect, nil
		case args[0] == "image":
			return engineImage, nil
		default:
			return "", nil
		}
	})
	fakeRegistry(t, "0.9.1")

	s := NewSmrti(engineConfig(t, true), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(context.Background(), rel, nil); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(*calls, " | ")
	for _, want := range []string{"rm -f smrti-0.8.2", "rm -f smrti-previous"} {
		if !strings.Contains(line, want) {
			t.Errorf("older generations were left behind: %q", line)
		}
	}
	if strings.Contains(line, "rm -f smrti-probe") {
		t.Errorf("a container that only shares the prefix was removed: %q", line)
	}
	// Once, clearing the name before the rename — never again afterwards.
	if strings.Count(line, "rm -f smrti-0.9.0") != 1 {
		t.Errorf("the rollback this swap just made must survive: %q", line)
	}
}

func TestSmrtiApplyWaitsForAQuietGraph(t *testing.T) {
	quickPacing(t)
	calls := engineDocker(t)
	fakeRegistry(t, "0.9.1")

	s := NewSmrti(engineConfig(t, true), func() bool { return false })
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(context.Background(), rel, nil)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(strings.Join(*calls, " "), "stop") {
		t.Errorf("a busy engine was stopped anyway: %v", *calls)
	}
}

func TestSmrtiApplyRollsBack(t *testing.T) {
	quickPacing(t)
	// The new container will not start.
	calls := engineDocker(t, "run")
	fakeRegistry(t, "0.9.1")

	s := NewSmrti(engineConfig(t, true), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(context.Background(), rel, nil)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error = %v", err)
	}
	line := strings.Join(*calls, " | ")
	for _, want := range []string{"rename smrti-0.9.0 smrti", "start smrti"} {
		if !strings.Contains(line, want) {
			t.Fatalf("docker calls %q missing %q", line, want)
		}
	}
}

func TestSmrtiApplyRollsBackWhenTheNewEngineIsSilent(t *testing.T) {
	quickPacing(t)
	calls := engineDocker(t)
	fakeRegistry(t, "0.9.1")

	// It starts, but never answers.
	s := NewSmrti(engineConfig(t, false), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(context.Background(), rel, nil)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(strings.Join(*calls, " | "), "rename smrti-0.9.0 smrti") {
		t.Fatalf("the engine that was working was not put back: %v", *calls)
	}
}

func TestSmrtiApplyReportsAFailedRollback(t *testing.T) {
	quickPacing(t)
	// Both the new container and the way back fail.
	engineDocker(t, "run", "rename smrti-0.9.0")
	fakeRegistry(t, "0.9.1")

	s := NewSmrti(engineConfig(t, true), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(context.Background(), rel, nil)
	if err == nil || !strings.Contains(err.Error(), "docker rename") {
		t.Fatalf("a rollback that cannot run must say how to finish it by hand: %v", err)
	}
}

func TestSmrtiUpdate(t *testing.T) {
	quickPacing(t)
	engineDocker(t)
	fakeRegistry(t, "0.9.1")
	s := NewSmrti(engineConfig(t, true), nil)

	var said []string
	out := func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }

	if err := s.Update(context.Background(), true, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(said, " "), "smrti 0.9.1 is available") {
		t.Fatalf("said %v", said)
	}

	said = nil
	if err := s.Update(context.Background(), false, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(said, " "), "upgraded smrti 0.9.0 to 0.9.1") {
		t.Fatalf("said %v", said)
	}

	// Nothing newer published: nothing done, and it says so.
	said = nil
	fakeRegistry(t, "0.9.0")
	if err := s.Update(context.Background(), false, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(said, " "), "0.9.0 is the newest") {
		t.Fatalf("said %v", said)
	}

	// A silent progress sink is fine.
	if err := s.Update(context.Background(), true, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLatestSmrtiTagNeedsAVersionedTag(t *testing.T) {
	fakeRegistry(t, "latest", "0.9", "main")
	if _, err := latestSmrtiTag(context.Background()); err == nil {
		t.Fatal("floating tags say nothing about which build they point at")
	}

	// A release candidate is not a release: it parses to the version it comes
	// before, and picking it would install something nobody published as done.
	fakeRegistry(t, "0.9.0", "0.9.1-rc1")
	got, err := latestSmrtiTag(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.9.0" {
		t.Errorf("latest tag = %q, want the newest finished release", got)
	}

	prev := registryBase
	registryBase = "http://127.0.0.1:0/nothing-listening"
	_, err = latestSmrtiTag(context.Background())
	registryBase = prev
	if err == nil {
		t.Fatal("an unreachable registry must be an error")
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/cyqlelabs/smrti:0.9.0": "0.9.0",
		"smrti:0.8.2":                   "0.8.2",
		"localhost:5000/smrti":          "",
		"smrti":                         "",
	}
	for ref, want := range cases {
		if got := imageTag(ref); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestSmrtiWatchReportsEachImageOnce(t *testing.T) {
	engineDocker(t)
	fakeRegistry(t, "0.9.1")

	ctx, cancel := context.WithCancel(context.Background())
	seen := make(chan SmrtiRelease, 4)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		NewSmrti(engineConfig(t, true), nil).Watch(ctx, 10*time.Millisecond, func(rel SmrtiRelease) {
			seen <- rel
		})
	}()
	select {
	case rel := <-seen:
		if rel.Version != "0.9.1" {
			t.Errorf("release = %+v", rel)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-stopped
		t.Fatal("the watcher never reported the newer image")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-stopped // the fakes it reads are torn down after this test returns
	if len(seen) > 0 {
		t.Errorf("the same image was reported %d more times", len(seen))
	}
}
