package llmguide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const root = "../.."

// The plugin skill is the guide plus its front matter: one source of truth.
func TestSkillBodyIsTheGuide(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root, "plugins/minion/skills/minion/SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.HasPrefix(s, "---\n") {
		t.Fatal("SKILL.md has no front matter")
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		t.Fatal("SKILL.md front matter is not closed")
	}
	if body := s[4+end+5:]; body != Text {
		t.Fatal("plugins/minion/skills/minion/SKILL.md differs from internal/llmguide/guide.md below its front matter: " +
			"regenerate it (front matter + guide.md)")
	}
}

// The help line names the command that prints the guide.
func TestHelpLineNamesTheCommand(t *testing.T) {
	if !strings.Contains(HelpLine, "minion llm-help") {
		t.Fatal(HelpLine)
	}
}

// Every plugin and marketplace manifest is valid JSON and names the plugin.
func TestManifestsAreValid(t *testing.T) {
	for _, p := range []string{
		".claude-plugin/marketplace.json",
		".agents/plugins/marketplace.json",
		"plugins/minion/.claude-plugin/plugin.json",
		"plugins/minion/.codex-plugin/plugin.json",
	} {
		raw, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if m["name"] != "minion" {
			t.Errorf("%s: name = %v", p, m["name"])
		}
	}
}

// The front matter is "name: minion" and a double-quoted description, so it is
// valid YAML whatever the description says (a bare "word: word" would not be).
func TestSkillFrontMatterIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root, "plugins/minion/skills/minion/SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(raw), "---\n", 3)
	if len(parts) != 3 || parts[0] != "" {
		t.Fatal("no front matter")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(parts[1]), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("front matter line %q is not key: value", line)
		}
		fields[k] = v
	}
	if fields["name"] != "minion" {
		t.Errorf("name = %q", fields["name"])
	}
	d, err := strconv.Unquote(fields["description"])
	if err != nil || len(d) < 50 || len(d) > 1024 {
		t.Errorf("description must be one double-quoted string of 50 to 1024 characters: %v (%d)", err, len(d))
	}
	if len(fields) != 2 {
		t.Errorf("front matter keys = %v", fields)
	}
}
