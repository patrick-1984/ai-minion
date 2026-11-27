package web

// The three permission boxes of a grant (v0.26.0): Receive files from
// target (files_pull), Send files to target (files_push) and Install apps on
// the target's Android Worker (apps_install). They sit directly beneath the
// level on the Add form and on Edit, start off, and are saved with the grant
// in the same step. Send and install require Control; Control ticks none.

import (
	"net/url"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// grantFlags are the boxes as a form sent them.
type grantFlags struct {
	Pull, Push, Apps bool
}

func formGrantFlags(f url.Values) grantFlags {
	return grantFlags{Pull: f.Get("files_pull") == "1", Push: f.Get("files_push") == "1", Apps: f.Get("apps_install") == "1"}
}

// refusal is the sentence for boxes the level does not allow; empty when the
// boxes can be sent.
func (g grantFlags) refusal(level string) string {
	if level == ops.GrantLevelControl {
		return ""
	}
	switch {
	case g.Push:
		return "Send files to target requires Control."
	case g.Apps:
		return "Install apps requires Control."
	}
	return ""
}

// permissionWords are the enabled boxes in words: "receive and send files",
// "install apps on pixel".
func (g grantFlags) permissionWords(device string) []string {
	var out []string
	switch {
	case g.Pull && g.Push:
		out = append(out, "receive and send files")
	case g.Pull:
		out = append(out, "receive files")
	case g.Push:
		out = append(out, "send files")
	}
	if g.Apps {
		if device == "" {
			device = "the target device"
		}
		out = append(out, "install apps on "+device)
	}
	return out
}

// grantLevelVerb is what a level lets the source do, as a verb.
func grantLevelVerb(level string) string {
	switch level {
	case ops.GrantLevelMessage:
		return "message"
	case ops.GrantLevelControl:
		return "control"
	}
	return "view"
}

// joinWords joins "a", "a and b", "a, b and c".
func joinWords(w []string) string {
	switch len(w) {
	case 0:
		return ""
	case 1:
		return w[0]
	}
	return strings.Join(w[:len(w)-1], ", ") + " and " + w[len(w)-1]
}

// grantSummarySentence is the one sentence of the Add form: "wb-2 can
// control minion-5 on home-pc and receive and send files."
func grantSummarySentence(from string, to []string, level string, flags grantFlags, device, until string) string {
	s := from + " can " + grantLevelVerb(level) + " " + joinWords(to)
	switch p := flags.permissionWords(device); len(p) {
	case 0:
	case 1:
		s += " and " + p[0]
	default:
		s += ", " + joinWords(p)
	}
	if until != "" {
		s += " " + until
	}
	return s + "."
}

// GrantBoxes are the three boxes beneath the level, on the Add form and on
// Edit. Push and Install are disabled below Control, with "Requires
// Control" beside them; Install apps is shown only for an Android target
// (on the Add form the script shows it once only Android targets are
// chosen).
type GrantBoxes struct {
	// Prefix starts the ids of the boxes.
	Prefix           string
	Pull, Push, Apps bool
	// Control is true while the level is Control.
	Control bool
	// OffersApps renders Install apps; Add keeps it hidden until the
	// script sees Android targets. Device names the device.
	OffersApps, Add bool
	Device          string
}

// AddBoxes are the boxes of the Add form: all off, at View.
func (GrantsPage) AddBoxes() GrantBoxes {
	return GrantBoxes{Prefix: "grant-add", OffersApps: true, Add: true}
}

// Boxes are the boxes of Edit, as the grant has them.
func (g GrantRow) Boxes() GrantBoxes {
	return GrantBoxes{Prefix: "grant-" + g.ID, Pull: g.FilesPull, Push: g.FilesPush, Apps: g.AppsInstall,
		Control: g.Level == ops.GrantLevelControl, OffersApps: g.OffersApps(), Device: g.TargetWorker()}
}
