package llmguide

import (
	"encoding/json"
	"os"
	"path/filepath"
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
