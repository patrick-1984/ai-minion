package agentdetect

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

var (
	claudeBackgroundTaskStatus = regexp.MustCompile(`(?:^|·)\s*[1-9]\d* background tasks?\s*(?:·|$)`)
	claudeBackgroundSummary    = regexp.MustCompile(`^[✢✳✶✻✽]\s+(?:Worked for .+ · [1-9]\d* shells? still running|Waiting for [1-9]\d* .+ to finish)$`)
	claudeBackgroundWorkflow   = regexp.MustCompile(`▱+\s+(\d+)/(\d+)\s*·\s*\d+[smh]`)
	claudeBackgroundAgentRow   = regexp.MustCompile(`^◯\s+\S.+\d+[smh]\b.+·\s*↓\s*[\d.]+k?\s+tokens\b`)
)

// ClaudeBackgroundWork looks for live background indicators in the prompt
// footer of Claude Code 2.1.33x. It is for display, independent of input readiness.
func ClaudeBackgroundWork(scr ops.Screen) bool {
	lines, _ := tail(scr.Lines, stateWindow, strings.TrimSpace)
	// Locate the lower rule of the input box. The status area is below it;
	// the current summary can sit immediately above the upper rule.
	lower := len(lines) - 1
	for lower >= 0 && !claudePromptRule(lines[lower]) {
		lower--
	}
	if lower < 2 {
		return false
	}
	upper := lower - 1
	for upper >= 0 && !claudePromptRule(lines[upper]) {
		upper--
	}
	if upper < 0 || upper+1 == lower || !strings.HasPrefix(lines[upper+1], "❯") {
		return false
	}
	if upper > 0 && claudeBackgroundSummary.MatchString(lines[upper-1]) {
		return true
	}
	for _, line := range lines[lower+1:] {
		if claudeBackgroundTaskStatus.MatchString(line) || claudeBackgroundAgentRow.MatchString(line) {
			return true
		}
		if progress := claudeBackgroundWorkflow.FindStringSubmatch(line); progress != nil {
			done, errDone := strconv.Atoi(progress[1])
			total, errTotal := strconv.Atoi(progress[2])
			if errDone == nil && errTotal == nil && done < total {
				return true
			}
		}
	}
	return false
}

// The upper rule may end in a workspace label or hint.
func claudePromptRule(line string) bool {
	return strings.HasPrefix(line, "────────")
}
