package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxRecordLine bounds one line of the trace file. A record with an absurd
// number of tool calls is still bounded by the turn's iteration limit, so a
// line past this is corruption rather than a busy turn, and skipping it beats
// letting one bad byte stop the reader.
const maxRecordLine = 1 << 20

// Since reads the records written on or after t, oldest first.
//
// The reader is deliberately forgiving: a half-written final line is what a
// crash mid-write leaves behind, and a watcher that refused to read anything
// because of it would go blind exactly when something has gone wrong.
func Since(dir string, t time.Time) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	cutoffDay := t.Format("2006-01-02")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		// Filenames are ISO dates, so a string compare is a date compare.
		if strings.TrimSuffix(name, ".jsonl") < cutoffDay {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)

	var out []Record
	for _, name := range files {
		recs, rerr := readFile(filepath.Join(dir, name), t)
		if rerr != nil {
			return out, rerr
		}
		out = append(out, recs...)
	}
	return out, nil
}

func readFile(path string, cutoff time.Time) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // a torn final line, not a reason to read nothing
		}
		if rec.Started.Before(cutoff) {
			continue
		}
		out = append(out, rec)
	}
	// A scanner error (an over-long line) ends this file and not the read.
	return out, nil
}

// Failed reports whether the turn ended in something that needs looking at.
// An interruption does not: the user talked over the answer, which is the
// system working.
func (r Record) Failed() bool { return r.Outcome == "error" }

// ToolErrors counts the tool calls that came back as failures.
func (r Record) ToolErrors() int {
	n := 0
	for _, t := range r.Tools {
		if t.Error {
			n++
		}
	}
	return n
}

// Count returns how many events of a kind the turn carried.
func (r Record) Count(kind string) int {
	n := 0
	for _, e := range r.Events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// CacheHitRate is the share of this turn's input that was served from cache.
func (r Record) CacheHitRate() float64 {
	if r.InputTokens <= 0 {
		return 0
	}
	return float64(r.CachedTokens) / float64(r.InputTokens)
}
