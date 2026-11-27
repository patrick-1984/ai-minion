package ops

import (
	"strings"
	"testing"
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
		Reason: "downgrade"})
	for _, want := range []string{"package=org.example.app", "version_code=7", "version=1.7_beta", "signer_sha256=" + strings.Repeat("ab", 32),
		"sha256=" + strings.Repeat("cd", 32), `reason="downgrade"`} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q lacks %q", d, want)
		}
	}
	if strings.Contains(d, "secret") {
		t.Errorf("detail %q names the path", d)
	}
}
