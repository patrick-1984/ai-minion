package api

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// app_install is a Worker op that mutates, so the dispatcher audits every
// request, denied ones included, with the outcome the Worker reported and the
// package, version and signer of the APK.
func TestAppInstallIsAudited(t *testing.T) {
	op, ok := ops.Lookup(ops.OpAppInstall)
	if !ok || !auditedMutation(op) {
		t.Fatalf("app_install is not audited by the dispatcher: %+v", op)
	}
	for _, c := range []struct{ outcome, want string }{
		{ops.AppInstallInstalled, "installed"},
		{ops.AppInstallNeedsConfirmation, "needs_confirmation_on_device"},
		{ops.AppInstallRefused, "refused"},
		{ops.AppInstallFailed, "failed"},
		{"", auditOutcomeError},
	} {
		r := &ops.AppInstallResult{Outcome: c.outcome}
		if got := outcomeOf(r); got != c.want {
			t.Errorf("outcome %q audited as %q, want %q", c.outcome, got, c.want)
		}
	}
	d := resultDetail(&ops.AppInstallResult{Outcome: ops.AppInstallInstalled, Package: "org.example.app", VersionCode: 3,
		SignerSHA256: strings.Repeat("ab", 32)})
	if !strings.Contains(d, "package=org.example.app") || !strings.Contains(d, "version_code=3") ||
		!strings.Contains(d, "signer="+strings.Repeat("ab", 32)) {
		t.Errorf("detail = %q", d)
	}
	// A refusal with a reason in words (and a version name with a space)
	// still fits the audit grammar: the row is written, not refused.
	for _, r := range []ops.AppInstallResult{
		{Outcome: ops.AppInstallRefused, Package: "org.example.app", VersionName: "1.2 beta", Reason: `the APK is signed with "another" key: a=b`},
		{Outcome: ops.AppInstallFailed, Reason: "Android said: INSTALL_FAILED_INSUFFICIENT_STORAGE"},
	} {
		d := proto.AppendDetail("proof=bound", resultDetail(r))
		if !audit.ValidDetail(d) || !strings.Contains(d, "reason=") {
			t.Errorf("detail %q is not a valid audit detail", d)
		}
	}
	if resultDetail(ops.WriteResult{}) != "" {
		t.Error("another result got an app install detail")
	}
}
