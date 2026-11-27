package ops

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestValidAppInstallPath(t *testing.T) {
	for _, p := range []string{"app.apk", "apps/Example.APK", "a b/c.apk"} {
		if err := ValidAppInstallPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "/abs.apk", "../x.apk", "a/../x.apk", "a//x.apk", "./x.apk", "x.txt", "x.apk/", "a\\b.apk", "x\x00.apk",
		strings.Repeat("a", AppInstallMaxPath) + ".apk"} {
		if err := ValidAppInstallPath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if err := (AppInstallArgs{WorkerID: "nope", Path: "x.apk"}).Validate(); err == nil {
		t.Error("a bad worker_id was accepted")
	}
}

func TestAppInstallActionImpliesNothing(t *testing.T) {
	if !ActionAppInstall.Valid() {
		t.Fatal("app_install is not a valid action")
	}
	op, ok := Lookup(OpAppInstall)
	if !ok || !op.ChangesState() || op.Scope != ScopeWorker {
		t.Fatalf("app_install = %+v", op)
	}
	if op.Actions.Permits(ActionOperate, ActionFileWrite, ActionManage, ActionRead, ActionMessage, ActionFileRead) {
		t.Fatal("app_install is permitted by another action")
	}
}

func TestAppInstallAuditDetail(t *testing.T) {
	d := AppInstallAuditDetail(AppInstallResult{Outcome: AppInstallRefused, Package: "org.example.app", VersionCode: 7,
		VersionName: "1.7 beta", SignerSHA256: strings.Repeat("ab", 32), SHA256: strings.Repeat("cd", 32), Path: "secret/name.apk",
		Reason: `the "version" is lower: 6 = installed`})
	for _, want := range []string{"package=org.example.app", "version_code=7", "signer=" + strings.Repeat("ab", 32),
		"reason=the__version__is_lower:_6___installed"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q lacks %q", d, want)
		}
	}
	if strings.Contains(d, "secret") {
		t.Errorf("detail %q names the path", d)
	}
	for _, pair := range strings.Split(d, " ") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || v == "" || strings.Trim(k, "abcdefghijklmnopqrstuvwxyz_") != "" || strings.ContainsAny(v, "=\"' ") {
			t.Errorf("pair %q of %q is outside the audit grammar", pair, d)
		}
	}
	// Long values are shortened to fit the audit field, the reason first.
	d = AppInstallAuditDetail(AppInstallResult{Outcome: AppInstallFailed, Package: strings.Repeat("p", 255), VersionCode: 1 << 40,
		SignerSHA256: strings.Repeat("ab", 32), Reason: strings.Repeat("why ", 100)})
	if n := len([]rune(d)); n > appInstallAuditBudget || !strings.Contains(d, "signer="+strings.Repeat("ab", 32)) {
		t.Errorf("long detail (%d runes) = %q", n, d)
	}
}

// An app op that names a Worker is sent on by the local daemon: it needs the
// forwarding daemon (app.install.forward), not app.install.v1 of the local
// one. Without a Worker it is the local Worker's own op.
func TestLocalRequiredFeaturesOfTheAppOps(t *testing.T) {
	w := NewUUIDv7()
	for _, tc := range []struct {
		op, args string
		want     string
	}{
		{OpAppInstall, `{"worker_id":"` + w + `","path":"x.apk"}`, proto.FeatureAppInstallForward},
		{OpAppInstallStatus, `{"worker_id":"` + w + `"}`, proto.FeatureAppInstallForward},
		{OpAppInstall, `{"path":"x.apk"}`, proto.FeatureAppInstallV1},
		{OpAppInstallStatus, `{}`, proto.FeatureAppInstallV1},
	} {
		if got := LocalRequiredFeatures(tc.op, json.RawMessage(tc.args)); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s %s: %v, want %s", tc.op, tc.args, got, tc.want)
		}
	}
	if got := RequiredFeatures(OpAppInstall, json.RawMessage(`{"worker_id":"`+w+`","path":"x.apk"}`)); len(got) != 1 || got[0] != proto.FeatureAppInstallV1 {
		t.Errorf("the Hub's check = %v", got)
	}
}
