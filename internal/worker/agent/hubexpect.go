package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// The expected Hub fingerprint (v0.11.0 trust ceremony). A Worker learns the
// Hub's fingerprint from the operator, never from the Hub: from the setting
// hub_fingerprint, from the answer to the prompt that comes before any
// network probe, or from the invitation key. The handshake then refuses any
// other Hub root before the code or any secret is used. A Worker that pairs
// without an expectation (an unattended first start, or `skip` at the prompt)
// is shown as "not verified" and can be checked later with `minion worker
// verify-hub` (the SSH known_hosts model: first use, or a key compared by
// entry).

// ErrPromptAborted is returned by [AskHubFingerprint] at the end of input.
var ErrPromptAborted = errors.New("aborted: nothing was changed")

// ParseHubExpectation parses the setting hub_fingerprint: "" gives the zero
// matcher (no expectation); anything else must be six words or 52
// characters. A malformed value is an error, never "absent".
func ParseHubExpectation(text string) (identity.FingerprintMatcher, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return identity.FingerprintMatcher{}, nil
	}
	m, err := identity.ParseFingerprintInput(text)
	if err != nil {
		return identity.FingerprintMatcher{}, fmt.Errorf("setting hub_fingerprint: %w", err)
	}
	return m, nil
}

// WriteHubFingerprintPrompt writes box M1, the question for the expected Hub
// fingerprint. The answer is read by the caller and judged by
// [HubFingerprintAnswer].
func WriteHubFingerprintPrompt(out io.Writer, hubURL string) {
	writeConsoleBox(out, "NEW HUB - ENTER ITS FINGERPRINT",
		"Hub address:  "+hubLabel(hubURL),
		"",
		"This Worker will pair only with the Hub that holds the key",
		"with the fingerprint you enter. The TLS certificate of the",
		"address does not prove which Hub answers: a proxy, or someone",
		"in between, can hold a valid certificate.",
		"",
		"Read the fingerprint on the Hub host, not on a web page:",
		"    minion hub identity --words",
		"    docker exec <hub container> minion hub identity --words",
		"",
		"Type all six words (the first four letters of each word are",
		"enough) or paste the 52 characters.",
		"Type skip to pair WITHOUT this check.",
	)
}

// HubFingerprintPrompt is the text before each answer to prompt M1.
const HubFingerprintPrompt = "Hub fingerprint: "

// HubFingerprintAnswer judges one answer to prompt M1. done is true for a
// final answer: a valid fingerprint (m is its matcher) or "skip" (m is the
// zero matcher). reply is the line to print ("" for an empty answer, which
// repeats the question).
func HubFingerprintAnswer(answer string) (m identity.FingerprintMatcher, done bool, reply string) {
	answer = strings.TrimSpace(answer)
	switch {
	case answer == "":
		return identity.FingerprintMatcher{}, false, ""
	case strings.EqualFold(answer, "skip"):
		return identity.FingerprintMatcher{}, true, `No fingerprint given: pairing without the check. The Hub will be shown as "not verified".`
	}
	m, err := identity.ParseFingerprintInput(answer)
	if err != nil {
		return identity.FingerprintMatcher{}, false, "That is not a fingerprint: " + strings.TrimPrefix(err.Error(), "identity: ") +
			". Type six words or 52 characters, or type skip."
	}
	return m, true, "Expected Hub fingerprint: " + m.Words()
}

// AskHubFingerprint shows prompt M1 and reads the expected Hub fingerprint.
// It is asked after the address was parsed and before any network probe. A
// valid answer returns its matcher; "skip" returns the zero matcher (pairing
// without the check); an empty line or an invalid answer repeats the
// question; the end of input is [ErrPromptAborted]. in is read without
// buffering, so input after the answer stays for the next reader.
func AskHubFingerprint(in io.Reader, out io.Writer, hubURL string) (identity.FingerprintMatcher, error) {
	WriteHubFingerprintPrompt(out, hubURL)
	for {
		_, _ = io.WriteString(out, HubFingerprintPrompt)
		line, err := readLine(in)
		if err != nil && line == "" {
			_, _ = io.WriteString(out, "\n")
			return identity.FingerprintMatcher{}, ErrPromptAborted
		}
		m, done, reply := HubFingerprintAnswer(line)
		if reply != "" {
			_, _ = fmt.Fprintln(out, reply)
		}
		if done {
			return m, nil
		}
		if err != nil {
			return identity.FingerprintMatcher{}, ErrPromptAborted
		}
	}
}

// readLine reads one line byte by byte (no read-ahead), without the line end.
// It returns io.EOF with what was read when the input ends first.
func readLine(in io.Reader) (string, error) {
	if in == nil {
		return "", io.EOF
	}
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() < 4096 {
		n, err := in.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimRight(b.String(), "\r"), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return b.String(), err
		}
	}
	return b.String(), nil
}

// HubCheckText is the text of a hub_check value in `status` and the boxes.
func HubCheckText(check string) string {
	switch check {
	case store.HubCheckFingerprint:
		return "matched the fingerprint you gave"
	case store.HubCheckInvite:
		return "from the invitation key"
	case store.HubCheckMismatch:
		return "DOES NOT MATCH the fingerprint you gave - not connecting"
	default:
		return "not verified"
	}
}

// hubCheckLine is HubCheckText for an enrollment, with the rotation note (the
// pin was reached from the checked key by a signed rotation) and the date.
func hubCheckLine(enr Enrollment) string {
	check := enr.Check()
	s := HubCheckText(check)
	if check == store.HubCheckNone {
		s += ": compare with the Hub host (minion hub identity --words), then run minion worker verify-hub <the 52 characters>"
	}
	if (check == store.HubCheckFingerprint || check == store.HubCheckInvite) && enr.HubCheckAnchor != "" &&
		!identity.Fingerprint(enr.HubCheckAnchor).Matches(enr.Pin.Pub) {
		s += "; the Hub key was reached by verified rotation from " + identity.Fingerprint(enr.HubCheckAnchor).Words()
	}
	if !enr.HubCheckAt.IsZero() {
		s += " (" + enr.HubCheckAt.UTC().Format(time.DateOnly) + ")"
	}
	return s
}

// boxLabelWidth is the width of the label column of the trust boxes.
const boxLabelWidth = 18

// boxWidth is the width of the text inside a console box.
const boxWidth = 60

// boxField returns label and value as box lines: the label padded to the label
// column, the value wrapped at the box width, continuation lines indented to
// the value column. The value is made terminal-safe first.
func boxField(label, value string) []string {
	pad := strings.Repeat(" ", boxLabelWidth)
	head := label
	if len(head) < boxLabelWidth {
		head += pad[len(head):]
	} else {
		head += " "
	}
	parts := boxWrap(proto.SafeText(value, 400), boxWidth-len(head))
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		if i == 0 {
			out = append(out, head+p)
			continue
		}
		out = append(out, pad+p)
	}
	return out
}

// boxWrap wraps text at width columns on spaces; a word longer than the width
// stands on a line of its own.
func boxWrap(text string, width int) []string {
	if width < 20 {
		width = 20
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	return append(lines, cur)
}

// fingerprintLines are the box lines of a fingerprint: the six words after
// the label, the 52 characters on the next line (indented less, so that the
// line fits the box).
func fingerprintLines(label string, f identity.Fingerprint) []string {
	return append(boxField(label, f.Words()), fullIndent+string(f))
}

// fullIndent is the indent of a 52-character fingerprint line in a box.
const fullIndent = "    "

func fingerprintOfPub(pub []byte) identity.Fingerprint {
	f, err := identity.FingerprintOf(pub)
	if err != nil {
		return ""
	}
	return f
}

// ErrNotPaired is returned by [VerifyHub] and [ForgetHub] for a Worker that
// is not paired with a Hub.
var ErrNotPaired = errors.New("this Worker is not paired with a Hub")

// hubMatch is how an expected fingerprint relates to an enrollment.
type hubMatch int

const (
	// matchNone: the expectation names neither the pin nor the checked key.
	matchNone hubMatch = iota
	// matchPin: the expectation names the pinned key.
	matchPin
	// matchAnchor: the expectation names the key this Worker was checked
	// with, and the pin was reached from it by a signed rotation chain (the
	// pin only moves along a verified chain, so a checked anchor that differs
	// from the pin is such a start; the TUF rule for root rotation).
	matchAnchor
)

func matchEnrolled(enr Enrollment, m identity.FingerprintMatcher) hubMatch {
	if m.Matches(enr.Pin.Pub) {
		return matchPin
	}
	check := enr.Check()
	if (check == store.HubCheckFingerprint || check == store.HubCheckInvite) && enr.HubCheckAnchor != "" &&
		m.MatchesFingerprint(identity.Fingerprint(enr.HubCheckAnchor)) {
		return matchAnchor
	}
	return matchNone
}

// MismatchBox prints box M6: the Hub this Worker is paired with does not match
// the fingerprint the operator gave (source is "You gave:" or "The setting
// hub_fingerprint says:").
func MismatchBox(out io.Writer, paired identity.Fingerprint, given identity.FingerprintMatcher, source string) {
	lines := []string{"This Worker is paired with:", fullIndent + paired.Words(), fullIndent + string(paired), source, fullIndent + given.Words()}
	if given.Full() {
		lines = append(lines, fullIndent+given.String())
	}
	lines = append(lines,
		"If the value you gave is from the Hub host, this Worker was",
		"paired with something else. It stops talking to that Hub now",
		"and stays disconnected.",
		"To pair it with your Hub:",
		"    minion worker pair --new-code --hub-fingerprint <value>",
		"and remove the old Worker entry in the Hub.",
		"If the value was wrong (another Hub, or the Hub's master key",
		"was rotated while this Worker was offline), give the right",
		"value with minion worker verify-hub <value>, or remove this",
		"pairing with:",
		"    minion worker verify-hub --forget --pinned "+string(paired),
	)
	writeConsoleBox(out, "HUB FINGERPRINT DOES NOT MATCH - WORKER DISCONNECTED", lines...)
}

// CheckEnrolledHub applies the setting hub_fingerprint to a paired Worker at
// start (SSH StrictHostKeyChecking): a match records the check; a value that
// names the key this Worker was checked with, from which the pin was reached
// by a signed rotation, connects with a Warn line to update the setting; any
// other value stores the mismatch (the connector does not connect, also after
// restarts) and prints box M6. A Worker that is not paired, or a zero m, is
// left alone.
func CheckEnrolledHub(ctx context.Context, e *Enrollments, m identity.FingerprintMatcher, out io.Writer, log *slog.Logger) error {
	if m.IsZero() {
		return nil
	}
	enr, ok, err := e.Load(ctx)
	if err != nil || !ok {
		return err
	}
	paired := fingerprintOfPub(enr.Pin.Pub)
	switch matchEnrolled(enr, m) {
	case matchPin:
		if enr.Check() != store.HubCheckFingerprint || enr.HubCheckAnchor != string(paired) {
			if err := e.SetHubCheck(ctx, enr.Pin, store.HubCheckFingerprint, string(paired)); err != nil {
				return err
			}
		}
		log.Info("the paired Hub matches hub_fingerprint", "hub_fingerprint_words", paired.Words())
	case matchAnchor:
		log.Warn("hub_fingerprint names the key this Worker was checked with; the Hub key has changed since by a signed "+
			"master key rotation. The Worker connects. Update the setting to "+string(paired),
			"setting", m.String(), "paired_with", string(paired), "paired_with_words", paired.Words())
	default:
		if enr.Check() != store.HubCheckMismatch {
			if err := e.SetHubCheck(ctx, enr.Pin, store.HubCheckMismatch, enr.HubCheckAnchor); err != nil {
				return err
			}
		}
		log.Error("the paired Hub does not match hub_fingerprint; not connecting",
			"paired_with", string(paired), "paired_with_words", paired.Words(), "setting", m.String(), "setting_words", m.Words())
		MismatchBox(out, paired, m, "The setting hub_fingerprint says:")
	}
	return nil
}

// VerifyHubResult is the outcome of [VerifyHub].
type VerifyHubResult struct {
	// Outcome is "match", "rotated" (the value names the key this Worker was
	// checked with; the pin was reached from it by a signed rotation) or
	// "mismatch".
	Outcome string
	// Paired is the pinned Hub fingerprint.
	Paired identity.Fingerprint
}

// Outcomes of [VerifyHub].
const (
	VerifyMatch    = "match"
	VerifyRotated  = "rotated"
	VerifyMismatch = "mismatch"
)

// VerifyHub is `minion worker verify-hub`: it compares the pinned Hub with m
// and stores the result (fingerprint, or mismatch, which stops the running
// Worker's connection within its epoch poll). [ErrNotPaired] when the Worker
// is not paired.
func VerifyHub(ctx context.Context, e *Enrollments, m identity.FingerprintMatcher) (VerifyHubResult, error) {
	enr, ok, err := e.Load(ctx)
	if err != nil {
		return VerifyHubResult{}, err
	}
	if !ok {
		return VerifyHubResult{}, ErrNotPaired
	}
	res := VerifyHubResult{Paired: fingerprintOfPub(enr.Pin.Pub)}
	switch matchEnrolled(enr, m) {
	case matchPin:
		res.Outcome = VerifyMatch
		err = e.SetHubCheck(ctx, enr.Pin, store.HubCheckFingerprint, string(res.Paired))
	case matchAnchor:
		res.Outcome = VerifyRotated
	default:
		res.Outcome = VerifyMismatch
		err = e.SetHubCheck(ctx, enr.Pin, store.HubCheckMismatch, enr.HubCheckAnchor)
	}
	if errors.Is(err, store.ErrNotEnrolled) {
		return VerifyHubResult{}, errors.New("the pairing changed while it was checked; run the command again")
	}
	return res, err
}

// ForgetHub is `minion worker verify-hub --forget --pinned <fingerprint>`
// (ssh-keygen -R): it removes the pairing whose pinned Hub key is exactly
// pinned, so the Worker must pair or enroll again; it never trusts the
// disputed key again. [ErrNotPaired] when the Worker is not paired,
// [ErrPinnedDiffers] when pinned is not the pinned key.
func ForgetHub(ctx context.Context, e *Enrollments, pinned identity.Fingerprint) error {
	err := e.ForgetPin(ctx, pinned)
	if errors.Is(err, store.ErrNotEnrolled) {
		return ErrNotPaired
	}
	return err
}
