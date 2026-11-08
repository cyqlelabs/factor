package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// writeMu serializes every write that judges a skill file by what it says
// before replacing it — induction, skill_write and quarantine — so none of
// them overwrites a version it never saw.
var writeMu sync.Mutex

// learnedKey is the frontmatter marker on skills Factor distilled from its
// own trajectories rather than had written for it. It is what lets induction
// update its own output later and what the learned-library cap counts — and
// it is deliberately absent from skill_write's output: a skill the model was
// asked to write, or rewrote over a learned one, is curated work now, and
// induction keeps its hands off it from then on.
const learnedKey = "learned"

// quarantineKey marks a learned skill taken out of the catalog because it
// kept going badly in use. The file stays where it is, so it can be read and
// judged: deleting the line puts the skill back, and so does induction or
// skill_write writing it again, since both write fresh frontmatter.
const quarantineKey = "quarantined"

// Quarantined reports whether a skill's frontmatter takes it out of the
// catalog.
func Quarantined(meta map[string]string) bool { return meta[quarantineKey] != "" }

// renderSkillDoc is the one place the SKILL.md wire format lives, so the two
// write paths cannot drift apart.
func renderSkillDoc(name, description, content string, learned bool) string {
	marker := ""
	if learned {
		marker = learnedKey + ": true\n"
	}
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n%s---\n\n%s\n", name, description, marker, content)
}

// WriteLearned writes root/name/SKILL.md carrying the learned marker,
// creating the skill or replacing a previously learned one. It refuses to
// replace a skill that exists without the marker: overwriting something a
// person wrote or installed with a machine distillation is the one mistake
// an unattended writer must not make. It reports whether the skill existed
// before the write.
func WriteLearned(root, name, description, content string) (string, bool, error) {
	if !validSkillName(name) {
		return "", false, fmt.Errorf("invalid skill name %q: letters, digits, - and _, starting with a letter or digit", name)
	}
	description = strings.Join(strings.Fields(description), " ")
	content = strings.TrimSpace(content)
	if description == "" || content == "" {
		return "", false, fmt.Errorf("skill %q needs both a description and a body", name)
	}
	// A model handing over a whole SKILL.md already wrote its own frontmatter;
	// wrapping it again stacks two blocks and the catalog reads the outer one.
	if _, body := parseFrontmatter(content); body != content {
		content = strings.TrimSpace(body)
	}

	dir := filepath.Join(root, name)
	path := filepath.Join(dir, "SKILL.md")
	writeMu.Lock()
	defer writeMu.Unlock()
	existed := false
	if data, err := os.ReadFile(path); err == nil {
		existed = true
		if meta, _ := parseFrontmatter(string(data)); meta[learnedKey] != "true" {
			return "", true, fmt.Errorf("skill %q was written or installed deliberately, not learned; leaving it alone", name)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", existed, fmt.Errorf("create skill dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(renderSkillDoc(name, description, content, true)), 0o644); err != nil {
		return "", existed, fmt.Errorf("write SKILL.md: %w", err)
	}
	return path, existed, nil
}

// Learned lists the skills under root that carry the learned marker — the
// only ones induction may replace and the ones the library cap counts.
func Learned(root string) []Skill {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		meta, body := parseFrontmatter(string(data))
		if meta[learnedKey] != "true" || Quarantined(meta) {
			continue
		}
		name := meta["name"]
		if name == "" {
			name = e.Name()
		}
		desc := meta["description"]
		if desc == "" {
			desc = firstParagraph(body)
		}
		out = append(out, Skill{Name: name, Description: desc, Path: path})
	}
	return out
}

// LearnedFile reports which learned skill path is the SKILL.md of — its
// directory name under root — and a hash of what the file says now. ok is
// false for any other file, for a skill somebody wrote, and for one already
// quarantined. Both paths must already be resolved the same way.
func LearnedFile(root, path string) (dir, hash string, ok bool) {
	if filepath.Base(path) != "SKILL.md" || filepath.Dir(filepath.Dir(path)) != filepath.Clean(root) {
		return "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	if meta, _ := parseFrontmatter(string(data)); meta[learnedKey] != "true" || Quarantined(meta) {
		return "", "", false
	}
	return filepath.Base(filepath.Dir(path)), contentHash(data), true
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}

// ErrSkillChanged is a quarantine refused because the skill is no longer the
// version that was judged: whatever replaced it has not been used yet.
var ErrSkillChanged = errors.New("the skill changed since it was judged")

// Quarantine takes the learned skill in root/dir out of the catalog and
// writes why into its frontmatter — but only while the file is still the
// version hash names, so a skill rewritten since it went wrong is not
// punished for its predecessor. A skill without the learned marker is
// refused: one somebody wrote or installed is theirs to judge.
func Quarantine(root, dir, hash, reason string) error {
	path := filepath.Join(root, dir, "SKILL.md")
	writeMu.Lock()
	defer writeMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(data)
	if meta, _ := parseFrontmatter(content); meta[learnedKey] != "true" {
		return fmt.Errorf("skill %q was not learned; leaving it alone", dir)
	}
	if contentHash(data) != hash {
		return ErrSkillChanged
	}
	reason = strings.Join(strings.Fields(reason), " ")
	content = "---\n" + quarantineKey + ": " + reason + "\n" + strings.TrimPrefix(content, "---\n")
	return os.WriteFile(path, []byte(content), 0o644)
}
