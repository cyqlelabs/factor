package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// watched loads a config file and starts a fast poll over it, reporting every
// change the watcher fires into the returned channel.
func watched(t *testing.T, path string) chan []string {
	t.Helper()
	baseline, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fired := make(chan []string, 4)
	go Watch(ctx, baseline, 5*time.Millisecond, func(_ *Config, sections []string) {
		fired <- sections
	})
	return fired
}

func writeConfig(t *testing.T, path string, mutate func(*Config)) {
	t.Helper()
	cfg := Default()
	if mutate != nil {
		mutate(cfg)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWatchReportsAChangedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, nil)
	fired := watched(t, path)

	writeConfig(t, path, func(c *Config) {
		c.Provider.Model = "another-model"
		c.Channels = map[string]json.RawMessage{"voice": json.RawMessage(`{"activation":"always"}`)}
	})
	select {
	case sections := <-fired:
		if !reflect.DeepEqual(sections, []string{"channels.voice", "provider"}) {
			t.Errorf("changed sections = %v", sections)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never noticed the change")
	}
}

// A half-saved edit must not take the running config down — or fire a reload.
// The watcher waits for the file to become loadable again and fires then.
func TestWatchRidesOutABrokenSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, nil)
	fired := watched(t, path)

	if err := os.WriteFile(path, []byte(`{"provider": {`), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case sections := <-fired:
		t.Fatalf("a broken save fired a change: %v", sections)
	case <-time.After(100 * time.Millisecond):
	}

	writeConfig(t, path, func(c *Config) { c.Provider.Model = "fixed" })
	select {
	case sections := <-fired:
		if !reflect.DeepEqual(sections, []string{"provider"}) {
			t.Errorf("changed sections = %v", sections)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never recovered from the broken save")
	}
}

// A rewrite that changes nothing — an editor saving the same content — is not
// a change, however much the mtime moved.
func TestWatchIgnoresATouchWithoutChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, nil)
	fired := watched(t, path)

	time.Sleep(20 * time.Millisecond) // let the mtime move
	writeConfig(t, path, nil)
	select {
	case sections := <-fired:
		t.Fatalf("an unchanged rewrite fired: %v", sections)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestChangedSections(t *testing.T) {
	base := Default()
	same := Default()
	if got := ChangedSections(base, same); len(got) != 0 {
		t.Errorf("identical configs differ: %v", got)
	}

	modified := Default()
	modified.Provider.Model = "other"
	modified.Heartbeat.Enabled = !base.Heartbeat.Enabled
	modified.Channels = map[string]json.RawMessage{"telegram": json.RawMessage(`{"token":"t"}`)}
	got := ChangedSections(base, modified)
	want := []string{"channels.telegram", "heartbeat", "provider"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedSections = %v, want %v", got, want)
	}
}
