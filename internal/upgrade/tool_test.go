package upgrade

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestToolContract(t *testing.T) {
	tool := &Tool{Current: "v0.3.0"}
	if tool.Name() != "upgrade" || tool.Description() == "" {
		t.Fatalf("name = %q", tool.Name())
	}
	props, _ := tool.Parameters()["properties"].(map[string]any)
	if _, ok := props["action"]; !ok {
		t.Fatalf("parameters = %v", tool.Parameters())
	}
}

func TestToolChecks(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("the new build")}.start(t)

	res := (&Tool{Current: "v0.3.0"}).Execute(context.Background(), nil)
	if res.IsError || !strings.Contains(res.ForLLM, "v0.4.0 is available") {
		t.Fatalf("result = %+v", res)
	}

	res = (&Tool{Current: "v0.4.0"}).Execute(context.Background(), map[string]any{"action": "check"})
	if res.IsError || !strings.Contains(res.ForLLM, "newest release") {
		t.Fatalf("result = %+v", res)
	}
}

func TestToolInstalls(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("the new build")}.start(t)
	exe := stageBinary(t, "the old build")

	res := (&Tool{Current: "v0.3.0"}).Execute(context.Background(), map[string]any{"action": "install"})
	if res.IsError || !strings.Contains(res.ForLLM, "next start") {
		t.Fatalf("result = %+v", res)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the new build" {
		t.Errorf("binary content = %q", got)
	}
}

func TestToolCoversTheMemoryEngine(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	quickPacing(t)
	release{tag: "v0.4.0", binary: []byte("the new build")}.start(t)
	engineDocker(t)
	fakeRegistry(t, "0.9.1")
	tool := &Tool{Current: "v0.4.0", Smrti: NewSmrti(engineConfig(t, true), nil)}

	res := tool.Execute(context.Background(), map[string]any{"action": "check"})
	if res.IsError || !strings.Contains(res.ForLLM, "smrti 0.9.1 is available") ||
		!strings.Contains(res.ForLLM, "factor v0.4.0 is the newest release") {
		t.Fatalf("a check covers both halves: %+v", res)
	}

	// A component nobody defined is both of them, not neither.
	res = tool.Execute(context.Background(), map[string]any{"component": "engine"})
	if res.IsError || !strings.Contains(res.ForLLM, "smrti 0.9.1") || !strings.Contains(res.ForLLM, "factor") {
		t.Fatalf("result = %+v", res)
	}

	res = tool.Execute(context.Background(), map[string]any{"action": "install", "component": "smrti"})
	if res.IsError || !strings.Contains(res.ForLLM, "Upgraded smrti from 0.9.0 to 0.9.1") {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(res.ForLLM, "factor") {
		t.Errorf("component=smrti must leave the binary alone: %q", res.ForLLM)
	}
}

func TestToolOnAnEngineItCannotUpgrade(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("the new build")}.start(t)
	// Docker is there, but smrti is not one of its containers.
	fakeDocker(t, func([]string) (string, error) { return "\n", nil })
	tool := &Tool{Current: "v0.4.0", Smrti: NewSmrti(engineConfig(t, true), nil)}

	// Asked about directly, it says why not.
	res := tool.Execute(context.Background(), map[string]any{"component": "smrti"})
	if !res.IsError || !strings.Contains(res.ForLLM, "does not run in a container here") {
		t.Fatalf("result = %+v", res)
	}

	// Asked in passing, a pip-installed engine is not news worth reporting.
	res = tool.Execute(context.Background(), nil)
	if res.IsError || strings.Contains(res.ForLLM, "container here") {
		t.Fatalf("result = %+v", res)
	}

	// And with memory off there is no engine at all.
	res = (&Tool{Current: "v0.4.0"}).Execute(context.Background(), map[string]any{"component": "smrti"})
	if res.IsError || !strings.Contains(res.ForLLM, "memory is off") {
		t.Fatalf("result = %+v", res)
	}
}

func TestToolReportsFailures(t *testing.T) {
	setPlatform(t, "linux", "amd64")

	prev := releaseAPI
	t.Setenv("FACTOR_HOME", t.TempDir())
	releaseAPI = "http://127.0.0.1:0/nothing-listening"
	res := (&Tool{Current: "v0.3.0"}).Execute(context.Background(), nil)
	releaseAPI = prev
	if !res.IsError {
		t.Fatalf("an unreachable API should be an error: %+v", res)
	}

	// A release whose checksums are missing must fail loudly rather than
	// leave the agent believing it upgraded.
	release{tag: "v0.4.0", binary: []byte("x"), assets: []string{AssetName()}}.start(t)
	stageBinary(t, "the old build")
	res = (&Tool{Current: "v0.3.0"}).Execute(context.Background(), map[string]any{"action": "install"})
	if !res.IsError || !strings.Contains(res.ForLLM, "SHA256SUMS") {
		t.Fatalf("result = %+v", res)
	}
}
