package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// learnedKey is the frontmatter marker on skills Factor distilled from its
// own trajectories rather than had written for it. It is what lets induction
// update its own output later and what the learned-library cap counts — and
// it is deliberately absent from skill_write's output: a skill the model was
// asked to write, or rewrote over a learned one, is curated work now, and
// induction keeps its hands off it from then on.
const learnedKey = "learned"

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
		if meta[learnedKey] != "true" {
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
