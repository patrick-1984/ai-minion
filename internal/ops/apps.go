package ops

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// App install on the Android Worker (v0.25.0, docs/guides/android-apps.md):
// the Worker installs another app from an APK that was copied into the app's
// "Minion files" folder with file transfer (files.v1), without adb.
//
// app_install is a state change of the Worker (Mutates: its result is the
// outcome, not a WriteResult). It needs the action app_install, which nothing
// implies: an administrator and the owner of the Worker hold it, an API key
// only when its scope names it, and a session of another Worker only through
// the apps_install flag of a verified Control grant or master acceptance
// (default off; never implied by files_push). The Hub audits every request
// with the package, the version, the signer and the outcome.
const (
	// OpAppInstall installs an APK from the Minion files folder.
	OpAppInstall = "app_install"
	// OpAppInstallStatus reports whether the app can install apps, the last
	// install attempt and the APK files at the top of the Minion files folder.
	OpAppInstallStatus = "app_install_status"
)

// AppInstallOpNames lists the app install ops, in catalog order.
var AppInstallOpNames = []string{OpAppInstall, OpAppInstallStatus}

// Limits of app_install.
const (
	// AppInstallMaxBytes is the largest APK the Worker hands to the app.
	AppInstallMaxBytes = 512 << 20
	// AppInstallMaxPath bounds the path argument in bytes.
	AppInstallMaxPath = 4096
	// AppInstallMaxListed bounds the APK files app_install_status lists.
	AppInstallMaxListed = 200
)

// Outcomes of app_install.
const (
	// AppInstallInstalled: Android installed the app.
	AppInstallInstalled = "installed"
	// AppInstallNeedsConfirmation: Android asks on the device (a first
	// install, or an app Minion did not install): one press of Install
	// <app> on the Minion screen.
	AppInstallNeedsConfirmation = "needs_confirmation_on_device"
	// AppInstallInstalling: the app is still checking or installing when the
	// call returns; app_install_status reports the outcome.
	AppInstallInstalling = "installing"
	// AppInstallRefused: a check refused the APK (reason says which).
	AppInstallRefused = "refused"
	// AppInstallFailed: Android or the handover failed (reason says why).
	AppInstallFailed = "failed"
)

// AppInstallArgs are the arguments of app_install.
type AppInstallArgs struct {
	// WorkerID names the Android Worker; required through the Hub.
	WorkerID string `json:"worker_id,omitempty"`
	// Path is the APK, relative to the Minion files folder.
	Path string `json:"path"`
	// AllowNew allows an app that is not installed yet (and the reinstall of
	// the installed version). Without it only an update of an installed app
	// with the same signer and a higher version is installed.
	AllowNew bool `json:"allow_new,omitempty"`
}

// Validate checks the Worker id and the shape of the path; the Worker applies
// the confinement of the Minion files folder.
func (a AppInstallArgs) Validate() error {
	if a.WorkerID != "" && !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	return ValidAppInstallPath(a.Path)
}

// ValidAppInstallPath checks a path relative to the Minion files folder:
// not empty, relative, no "." or ".." elements, no empty element, an .apk
// file name, no NUL or backslash.
func ValidAppInstallPath(p string) error {
	switch {
	case p == "":
		return invalid("path", "is required (the APK, relative to the Minion files folder)")
	case len(p) > AppInstallMaxPath:
		return invalid("path", "is longer than %d bytes", AppInstallMaxPath)
	case strings.HasPrefix(p, "/"):
		return invalid("path", "must be relative to the Minion files folder")
	case strings.ContainsAny(p, "\x00\\"):
		return invalid("path", "must not contain NUL or a backslash")
	}
	for _, el := range strings.Split(p, "/") {
		if el == "" || el == "." || el == ".." {
			return invalid("path", "must not contain empty, \".\" or \"..\" elements")
		}
	}
	if !strings.HasSuffix(strings.ToLower(p), ".apk") {
		return invalid("path", "must name an .apk file")
	}
	return nil
}

// AppInstallResult is the result of app_install, and the last attempt of
// app_install_status. It never holds file contents.
type AppInstallResult struct {
	// RequestID names the attempt.
	RequestID string `json:"request_id,omitempty"`
	// Outcome is one of the AppInstall* outcomes.
	Outcome string `json:"outcome"`
	// Reason explains a refusal, a failure or what to do on the device.
	Reason string `json:"reason,omitempty"`
	// Path is the APK relative to the Minion files folder.
	Path string `json:"path,omitempty"`
	// SHA256 and Size are those of the APK the Worker handed over.
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// Package, VersionCode, VersionName and Label are what the APK says
	// (empty when the app did not read it).
	Package     string `json:"package,omitempty"`
	VersionCode int64  `json:"version_code,omitempty"`
	VersionName string `json:"version_name,omitempty"`
	Label       string `json:"label,omitempty"`
	// SignerSHA256 is the SHA-256 of the APK's signing certificate.
	SignerSHA256 string `json:"signer_sha256,omitempty"`
	// InstalledVersionCode is the version code installed before (0 none).
	InstalledVersionCode int64 `json:"installed_version_code,omitempty"`
	// NewInstall says the app was not installed before.
	NewInstall bool `json:"new_install,omitempty"`
}

// AppInstallAuditDetail is the detail of the Hub's audit row of an app
// install: the package, the version, the signer and the digest of the APK, and
// the reason of an outcome other than installed. It never holds a path or
// file contents.
func AppInstallAuditDetail(r AppInstallResult) string {
	word := func(s string, n int) string {
		if s == "" {
			return "none"
		}
		return proto.SafeText(strings.Join(strings.Fields(s), "_"), n)
	}
	d := "package=" + word(r.Package, 255) + " version_code=" + strconv.FormatInt(r.VersionCode, 10) +
		" version=" + word(r.VersionName, 60) + " signer_sha256=" + word(r.SignerSHA256, 200) +
		" sha256=" + word(r.SHA256, 64) + " new_install=" + strconv.FormatBool(r.NewInstall)
	if r.Reason != "" && r.Outcome != AppInstallInstalled {
		d += " reason=" + strconv.Quote(proto.SafeText(r.Reason, 200))
	}
	return d
}

// AppInstallStatusArgs are the arguments of app_install_status.
type AppInstallStatusArgs struct {
	WorkerID string `json:"worker_id,omitempty"`
}

// Validate checks the Worker id.
func (a AppInstallStatusArgs) Validate() error {
	if a.WorkerID != "" && !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	return nil
}

// AppInstallFile is one APK file at the top of the Minion files folder.
type AppInstallFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// AppInstallStatus is the result of app_install_status.
type AppInstallStatus struct {
	// Ready says whether the app can install apps now; BlockReason why not.
	Ready       bool   `json:"ready"`
	BlockReason string `json:"block_reason,omitempty"`
	// Last is the last attempt the app reported, nil without one.
	Last *AppInstallResult `json:"last,omitempty"`
	// Files are the .apk files at the top of the Minion files folder (at most
	// AppInstallMaxListed, by name).
	Files []AppInstallFile `json:"files"`
}

func init() {
	install := MustActionSet(ActionAppInstall)
	Register(Op{
		Name: OpAppInstall,
		Summary: "Install an app on an Android Worker from an APK in its Minion files folder (owner or admin, or a session with the apps_install flag); " +
			"a first install needs one confirmation on the device.",
		Scope: ScopeWorker, Actions: install, Mutates: true,
		Args: reflect.TypeFor[AppInstallArgs](), Result: reflect.TypeFor[AppInstallResult](),
	})
	Register(Op{
		Name:    OpAppInstallStatus,
		Summary: "Show whether an Android Worker can install apps, its last app install and the APK files in its Minion files folder.",
		Scope:   ScopeWorker, Actions: install,
		Args: reflect.TypeFor[AppInstallStatusArgs](), Result: reflect.TypeFor[AppInstallStatus](),
	})
}
