package agentdetect

import (
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// The anchor and extraction core of the turn extractor (spine AM-5): pure
// decisions over recorded evidence. Nothing here reads a screen to find where
// a reply starts; it checks that the evidence still holds and reads the rows
// the evidence points at.

// markReason returns the reason a delivery mark cannot anchor a turn, or "".
func markReason(m term.DeliveryMark) string {
	switch {
	case m.Height < 1 || m.Width < 1:
		return ReasonNoEvidence
	case !m.HasCounters():
		return ReasonTmuxTooOld
	case m.Alternate:
		return ReasonAlternateScreen
	case m.RegionUpper > 0:
		return ReasonScrollRegion
	}
	top := m.ScreenTop()
	if m.From < m.Collected || m.From > top || int64(len(m.Rows)) != m.LastRow()-m.From+1 {
		return ReasonNoEvidence
	}
	return ""
}

// frameReason checks the evidence of a frame against the evidence at delivery.
// It returns the reason that retires the delivery, or skip for a frame that
// must not be read at all (the alternate screen is on). The order matters: a
// width change also raises the generation, and the width is the more precise
// reason.
func frameReason(d, f term.PaneEvidence) (reason string, skip bool) {
	switch {
	case !f.HasCounters():
		return ReasonTmuxTooOld, false
	case f.Dead || f.PID != d.PID || f.Command != d.Command:
		// Not the process of the delivery. A pid alone is not enough: an agent
		// that exits back to its shell in the middle of a reply keeps the pid
		// of the pane, and the rows of the shell are not the reply.
		return ReasonPaneRespawned, false
	case f.Alternate:
		return "", true
	case f.Width != d.Width:
		return ReasonResized, false
	case f.Generation != d.Generation:
		return ReasonHistoryCleared, false
	case f.RegionUpper > 0:
		return ReasonScrollRegion, false
	}
	return "", false
}

// anchorResult is the outcome of looking for the echo in a frame.
type anchorResult struct {
	ok      bool   // the echo is verified and the extractor is anchored
	changed bool   // a row above the bottom chrome differs from the delivery
	reason  string // the delivery is retired for this reason
}

// baseAt returns the row of the delivery at absolute index idx. Rows below the
// screen of the delivery are blank.
func (e *TurnExtractor) baseAt(idx int64) string {
	if i := idx - e.top0; i >= 0 && i < int64(len(e.base)) {
		return e.base[i]
	}
	return ""
}

// markAt returns the row of the delivery mark at absolute index idx; rows
// outside the mark are blank (rows below the screen of the delivery).
func (e *TurnExtractor) markAt(idx int64) string {
	if i := idx - e.mfrom; i >= 0 && i < int64(len(e.mrows)) {
		return e.mrows[i]
	}
	return ""
}

// tryAnchor verifies the echo of the delivered message at the first row that
// changed since delivery. rows start at absolute index start; endIdx is the
// first row of the frame's bottom chrome.
//
// f is the first absolute index, from the top of the screen at delivery on,
// whose text differs from the delivery. The echo must verify there (blank and
// border rows at f belong to a boxed echo and are skipped) and above the
// chrome. When f lies above the chrome at delivery, the echo rewrote rows that
// were transcript then, and an identical echo at an unchanged row above f
// would make the start ambiguous. On success the extractor is anchored: the
// start is fixed and never searched for again.
func (e *TurnExtractor) tryAnchor(rows []string, start, top, last, endIdx int64) anchorResult {
	f := int64(-1)
	for idx := e.top0; idx <= last; idx++ {
		if rows[idx-start] != e.baseAt(idx) {
			f = idx
			break
		}
	}
	if f < 0 || f >= endIdx {
		// Nothing changed above the chrome: the agent has not printed yet (a
		// draft in the input box is chrome).
		return anchorResult{}
	}
	res := anchorResult{changed: true}
	ep := f
	for ep < endIdx && isBlankOrBorder(rows[ep-start]) {
		ep++
	}
	if ep >= endIdx {
		return res
	}
	bare, modes := bareEcho[e.agent.Name], modePrompts[e.agent.Name]
	above := rows[:endIdx-start] // every row from start to the chrome
	j, ok := matchEcho(above, int(ep-start), e.sent, bare, modes)
	if !ok {
		return res
	}
	if f < e.chrome0 {
		// Rows that were transcript at delivery changed. Could the message
		// also be an echo at an unchanged row between the top of the screen
		// at delivery and f? Then the reply may start there instead.
		unchanged := rows[e.top0-start : f-start]
		for s := range unchanged {
			if _, ok := matchEcho(unchanged, s, e.sent, bare, modes); ok {
				res.reason = ReasonEchoAmbiguous
				return res
			}
		}
	}
	e.anchored = true
	e.echoFrom, e.echoTo = ep, start+int64(j)
	e.echoRows = append([]string(nil), rows[ep-start:e.echoTo-start]...)
	e.reprint = e.reprintPattern(rows, start, f, ep)
	e.histFrom, e.final = e.top0, e.top0
	e.hist = nil
	res.ok = true
	return res
}

// reprintPattern returns the rows above the echo followed by the echo, or nil
// when the rows above hold nothing but blanks (nothing to tell a reprint by).
// Rows above f did not change since delivery and come from the mark.
func (e *TurnExtractor) reprintPattern(rows []string, start, f, ep int64) []string {
	from := max(e.mfrom, ep-reprintContext)
	var above []string
	content := false
	for idx := from; idx < ep; idx++ {
		var r string
		if idx < f {
			r = e.markAt(idx)
		} else {
			r = rows[idx-start]
		}
		above = append(above, r)
		content = content || !isBlankRow(r)
	}
	if !content {
		return nil
	}
	return append(above, e.echoRows...)
}

// follow reads one frame into the anchored reply: it checks that the rows
// already read still hold, moves the rows that became history into hist, and
// takes the reply from the echo to the bottom chrome. It returns the reason
// that retires the delivery, or "".
func (e *TurnExtractor) follow(rows []string, start, top, last, endIdx int64) string {
	// A consumed row that the pane no longer holds (a pane that shrank after
	// its rows were erased drops them) cannot be told from a row that was
	// rewritten: the evidence is gone.
	if e.final > last+1 {
		return ReasonEvidenceMismatch
	}
	// A row that was consumed as history and is visible again (the pane grew)
	// must still read the same: a change would be a rewrite of rows that were
	// final.
	for idx := max(start, e.histFrom); idx < min(e.final, last+1); idx++ {
		if i := idx - e.histFrom; i < int64(len(e.hist)) && rows[idx-start] != e.hist[i] {
			return ReasonEvidenceMismatch
		}
	}
	// The echo is a row read once at its index. A quote that overwrote it in
	// place is not the echo any more.
	for idx := e.echoFrom; idx < e.echoTo; idx++ {
		if idx >= start && idx <= last && rows[idx-start] != e.echoRows[idx-e.echoFrom] {
			return ReasonEvidenceMismatch
		}
	}
	// History is final: read once.
	for idx := e.final; idx < top; idx++ {
		if len(e.hist) >= maxTrackedLines {
			e.capped = true
			continue
		}
		e.hist = append(e.hist, rows[idx-start])
	}
	if top > e.final {
		e.final = top
	}

	// The reply: from the echo to the bottom chrome.
	at := func(idx int64) (string, bool) {
		if idx < e.final && idx >= e.histFrom {
			i := idx - e.histFrom
			if i >= int64(len(e.hist)) {
				return "", false // dropped at maxTrackedLines
			}
			return e.hist[i], true
		}
		if idx >= start && idx <= last {
			return rows[idx-start], true
		}
		return "", false
	}
	var reply []string
	for idx := e.echoTo; idx < endIdx; idx++ {
		r, ok := at(idx)
		if !ok {
			break
		}
		reply = append(reply, r)
	}
	if len(e.reprint) > 0 && containsRows(reply, e.reprint) {
		return ReasonTranscriptReprinted
	}
	skip := 0
	for skip < len(reply) && isBlankOrBorder(reply[skip]) {
		skip++
	}
	e.reply = reply[skip:]
	return ""
}

func isBlankRow(l string) bool {
	for _, r := range l {
		if r != ' ' && r != '\t' {
			return false
		}
	}
	return true
}

// containsRows reports whether pat occurs in rows as a run of equal rows.
func containsRows(rows, pat []string) bool {
	for i := 0; i+len(pat) <= len(rows); i++ {
		match := true
		for j := range pat {
			if rows[i+j] != pat[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
