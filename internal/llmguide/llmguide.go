// Package llmguide holds the operating guide for AI agents: what
// "minion llm-help" prints. The same text is the body of the Minion skill of
// the Claude Code and Codex plugin (plugins/minion/skills/minion/SKILL.md); a
// test keeps the two equal, so there is one source.
package llmguide

import _ "embed"

// Text is the guide, Markdown.
//
//go:embed guide.md
var Text string

// HelpLine is the first line of "minion --help": it sends an agent to the guide.
const HelpLine = "If you are an LLM or AI agent: run \"minion llm-help\" first; it explains how to operate Minion."
