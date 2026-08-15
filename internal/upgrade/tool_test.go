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

func TestToolReportsFailures(t *testing.T) {
	setPlatform(t, "linux", "amd64")

	prev := releaseAPI
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
