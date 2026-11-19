package service

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	ksvc "github.com/kardianos/service"

	"github.com/patrick-1984/ai-minion/internal/version"
)

const (
	userSID      = "S-1-5-21-1004336348-1177238915-682003330-1001"
	pfMinionDir  = `C:\Program Files\Minion`
	pfMinionExe  = `C:\Program Files\Minion\minion.exe`
	userWinExe   = `C:\Users\alice\AppData\Local\Programs\minion\minion.exe`
	fullControl  = 0x1f01ff
	readExecute  = 0x1200a9
	modifyRights = 0x1301bf
	testSum      = "3f2a8c1d0e4b5a6978c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"
)

func TestServiceBinaryPathIsBelowProgramFiles(t *testing.T) {
	for _, pf := range []string{`C:\Program Files`, `C:\Program Files\`, `D:\Apps/`} {
		got := ServiceBinaryPath(pf)
		if !strings.HasSuffix(got, `\Minion\minion.exe`) || strings.Contains(got, `\\`) {
			t.Errorf("ServiceBinaryPath(%q) = %q", pf, got)
		}
	}
	if got := ServiceBinaryDir(`C:\Program Files`); got != pfMinionDir {
		t.Errorf("ServiceBinaryDir = %q", got)
	}
}

func TestSameWindowsPath(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{pfMinionExe, `c:\program files\minion\MINION.EXE`, true},
		{pfMinionExe, `C:/Program Files/Minion/minion.exe`, true},
		{pfMinionExe, `\\?\C:\Program Files\Minion\minion.exe`, true},
		{pfMinionDir, pfMinionDir + `\`, true},
		{pfMinionExe, userWinExe, false},
		{"", "", false},
	} {
		if got := SameWindowsPath(c.a, c.b); got != c.want {
			t.Errorf("SameWindowsPath(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestParseServiceImagePath(t *testing.T) {
	for in, want := range map[string]string{
		`"C:\Program Files\Minion\minion.exe" worker service-run`: pfMinionExe,
		`"C:\Program Files\Minion\minion.exe"`:                    pfMinionExe,
		`C:\Users\a\minion.exe worker service-run`:                `C:\Users\a\minion.exe`,
		`C:\Program Files\Minion\minion.EXE worker service-run`:   `C:\Program Files\Minion\minion.EXE`,
		`C:\tools\minion`:             `C:\tools\minion`,
		`"C:\unterminated\minion.exe`: `C:\unterminated\minion.exe`,
	} {
		if got := ParseServiceImagePath(in); got != want {
			t.Errorf("ParseServiceImagePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBinaryChainRolesUpToTheRoot(t *testing.T) {
	got := BinaryChain(pfMinionExe)
	want := []ChainStep{
		{pfMinionExe, RoleFile},
		{pfMinionDir, RoleDir},
		{`C:\Program Files`, RoleAncestor},
		{`C:\`, RoleAncestor},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %+v", got)
	}
	unc := BinaryChain(`\\srv\share\bin\minion.exe`)
	if len(unc) != 3 || unc[2].Path != `\\srv\share\` || unc[1].Role != RoleDir {
		t.Errorf("UNC chain = %+v", unc)
	}
}

// The security descriptors below are what Windows really has on these paths
// (simplified to the ACEs that matter).
var (
	// %ProgramFiles%\Minion as the install leaves it.
	sdServiceDir = SecurityInfo{Owner: sidAdministrators, DACL: []ACE{
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit, Mask: fullControl, SID: sidSystem},
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit, Mask: fullControl, SID: sidAdministrators},
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit, Mask: readExecute, SID: sidUsers},
	}}
	sdServiceFile = SecurityInfo{Owner: sidAdministrators, DACL: []ACE{
		{Type: aceAllowed, Mask: fullControl, SID: sidSystem},
		{Type: aceAllowed, Mask: fullControl, SID: sidAdministrators},
		{Type: aceAllowed, Mask: readExecute, SID: sidUsers},
	}}
	// C:\Program Files: TrustedInstaller owns it; CREATOR OWNER is inherit-only.
	sdProgramFiles = SecurityInfo{Owner: sidTrustedInstall, DACL: []ACE{
		{Type: aceAllowed, Mask: fullControl, SID: sidTrustedInstall},
		{Type: aceAllowed, Mask: modifyRights, SID: sidSystem},
		{Type: aceAllowed, Mask: modifyRights, SID: sidAdministrators},
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit | aceInheritOnly, Mask: rightGenericAll, SID: sidCreatorOwner},
		{Type: aceAllowed, Mask: readExecute, SID: sidUsers},
	}}
	// C:\ : Authenticated Users may create folders (harmless above the binary).
	sdDriveRoot = SecurityInfo{Owner: sidSystem, DACL: []ACE{
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit, Mask: fullControl, SID: sidAdministrators},
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit, Mask: fullControl, SID: sidSystem},
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit, Mask: readExecute, SID: sidUsers},
		{Type: aceAllowed, Mask: rightAppendData, SID: sidAuthenticated},
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit | aceInheritOnly, Mask: rightDelete | rightGenericWrite, SID: sidAuthenticated},
		{Type: aceAllowed, Flags: aceContainerInherit, Mask: rightAppendData, SID: sidUsers},
	}}
	// %LOCALAPPDATA%\Programs\minion and the binary the installer put there.
	sdUserDir = SecurityInfo{Owner: userSID, DACL: []ACE{
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit, Mask: fullControl, SID: sidSystem},
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit, Mask: fullControl, SID: sidAdministrators},
		{Type: aceAllowed, Flags: aceObjectInherit | aceContainerInherit, Mask: fullControl, SID: userSID},
	}}
)

func TestUntrustedWritersAcceptsTheServiceLayout(t *testing.T) {
	for name, c := range map[string]struct {
		sd   SecurityInfo
		role PathRole
	}{
		"file":          {sdServiceFile, RoleFile},
		"dir":           {sdServiceDir, RoleDir},
		"program files": {sdProgramFiles, RoleAncestor},
		"drive root":    {sdDriveRoot, RoleAncestor},
	} {
		if got := UntrustedWriters(c.sd, c.role); len(got) != 0 {
			t.Errorf("%s: writers = %v", name, got)
		}
	}
}

func TestUntrustedWritersFindsTheUserOfAnInstallerPath(t *testing.T) {
	got := UntrustedWriters(sdUserDir, RoleDir)
	if !reflect.DeepEqual(got, []string{userSID}) {
		t.Errorf("writers = %v", got)
	}
	file := SecurityInfo{Owner: userSID, DACL: []ACE{{Type: aceAllowed, Mask: fullControl, SID: userSID}}}
	if got := UntrustedWriters(file, RoleFile); !reflect.DeepEqual(got, []string{userSID}) {
		t.Errorf("file writers = %v", got)
	}
}

func TestUntrustedWritersRules(t *testing.T) {
	adminOwned := func(aces ...ACE) SecurityInfo { return SecurityInfo{Owner: sidAdministrators, DACL: aces} }
	for name, c := range map[string]struct {
		sd   SecurityInfo
		role PathRole
		want []string
	}{
		"null DACL":                           {SecurityInfo{Owner: sidAdministrators, NullDACL: true}, RoleFile, []string{sidNullDACLLabel}},
		"admin owner, no ACEs":                {adminOwned(), RoleFile, nil},
		"user owner":                          {SecurityInfo{Owner: userSID}, RoleAncestor, []string{userSID}},
		"no owner":                            {SecurityInfo{}, RoleFile, []string{sidNoOwnerSentinel}},
		"owner rights limit":                  {SecurityInfo{Owner: userSID, DACL: []ACE{{Type: aceAllowed, Mask: readExecute, SID: sidOwnerRights}}}, RoleFile, nil},
		"owner rights grant":                  {SecurityInfo{Owner: userSID, DACL: []ACE{{Type: aceAllowed, Mask: rightWriteDAC, SID: sidOwnerRights}}}, RoleFile, []string{userSID}},
		"users write data":                    {adminOwned(ACE{Type: aceAllowed, Mask: rightWriteData, SID: sidUsers}), RoleFile, []string{sidUsers}},
		"users delete file":                   {adminOwned(ACE{Type: aceAllowed, Mask: rightDelete, SID: sidUsers}), RoleFile, []string{sidUsers}},
		"users add file":                      {adminOwned(ACE{Type: aceAllowed, Mask: rightWriteData, SID: sidUsers}), RoleDir, []string{sidUsers}},
		"add file above":                      {adminOwned(ACE{Type: aceAllowed, Mask: rightWriteData | rightAppendData, SID: sidUsers}), RoleAncestor, nil},
		"delete child above":                  {adminOwned(ACE{Type: aceAllowed, Mask: rightDeleteChild, SID: sidInteractive}), RoleAncestor, []string{sidInteractive}},
		"generic all":                         {adminOwned(ACE{Type: aceAllowed, Mask: rightGenericAll, SID: sidEveryone}), RoleAncestor, []string{sidEveryone}},
		"deny is ignored":                     {adminOwned(ACE{Type: aceDenied, Mask: fullControl, SID: sidUsers}, ACE{Type: aceAllowed, Mask: fullControl, SID: sidUsers}), RoleFile, []string{sidUsers}},
		"deny alone":                          {adminOwned(ACE{Type: aceDenied, Mask: fullControl, SID: sidUsers}), RoleFile, nil},
		"unsupported ACE":                     {adminOwned(ACE{Type: 0x5, Mask: fullControl}), RoleFile, []string{sidUnsupportedACE}},
		"inherit-only on file":                {adminOwned(ACE{Type: aceAllowed, Flags: aceInheritOnly | aceObjectInherit, Mask: fullControl, SID: sidUsers}), RoleFile, nil},
		"inherit-only on dir":                 {adminOwned(ACE{Type: aceAllowed, Flags: aceInheritOnly | aceObjectInherit, Mask: rightWriteData, SID: sidUsers}), RoleDir, []string{sidUsers}},
		"inherit-only, files only, dir right": {adminOwned(ACE{Type: aceAllowed, Flags: aceInheritOnly | aceObjectInherit, Mask: rightDeleteChild, SID: sidUsers}), RoleDir, nil},
		"inherit-only above":                  {adminOwned(ACE{Type: aceAllowed, Flags: aceInheritOnly | aceObjectInherit | aceContainerInherit, Mask: fullControl, SID: sidUsers}), RoleAncestor, nil},
		"creator owner on dir":                {adminOwned(ACE{Type: aceAllowed, Flags: aceInheritOnly | aceObjectInherit, Mask: fullControl, SID: sidCreatorOwner}), RoleDir, nil},
		"trusted installer":                   {SecurityInfo{Owner: strings.ToLower(sidTrustedInstall)}, RoleFile, nil},
		"duplicates collapse":                 {adminOwned(ACE{Type: aceAllowed, Mask: rightWriteData, SID: sidUsers}, ACE{Type: aceAllowed, Mask: rightDelete, SID: sidUsers}), RoleFile, []string{sidUsers}},
	} {
		got := UntrustedWriters(c.sd, c.role)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: writers = %v, want %v", name, got, c.want)
		}
	}
}

func TestFindingsForWalksTheChainAndWarns(t *testing.T) {
	sds := map[string]SecurityInfo{
		pfMinionExe:        sdServiceFile,
		pfMinionDir:        sdServiceDir,
		`C:\Program Files`: sdProgramFiles,
		`C:\`:              sdDriveRoot,
	}
	read := func(p string) (SecurityInfo, error) {
		sd, ok := sds[p]
		if !ok {
			return SecurityInfo{}, errors.New("unexpected path " + p)
		}
		return sd, nil
	}
	f, err := FindingsFor(pfMinionExe, read)
	if err != nil || len(f) != 0 {
		t.Fatalf("findings = %v, err = %v", f, err)
	}
	if w := ServiceBinaryWarning(pfMinionExe, f); w != "" {
		t.Errorf("warning for a safe binary: %q", w)
	}

	sds[pfMinionDir] = sdUserDir
	f, err = FindingsFor(pfMinionExe, read)
	if err != nil || len(f) != 1 || f[0].SID != userSID || f[0].Path != pfMinionDir {
		t.Fatalf("findings = %+v, err = %v", f, err)
	}
	w := ServiceBinaryWarning(pfMinionExe, append(f, BinaryFinding{Path: pfMinionExe, SID: sidUsers}))
	for _, want := range []string{"writable by non-administrators", pfMinionExe, userSID, `BUILTIN\Users`, "Do not run that file elevated", "MINION_INSTALL_SERVICE", "install.ps1", "LocalSystem"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %s", want, w)
		}
	}

	if _, err := FindingsFor(`C:\elsewhere\minion.exe`, read); err == nil || !strings.Contains(err.Error(), `C:\elsewhere\minion.exe`) {
		t.Errorf("unreadable path: err = %v", err)
	}
}

func TestServiceBinaryWarningShortensLongLists(t *testing.T) {
	var f []BinaryFinding
	for _, p := range []string{`C:\a`, `C:\b`, `C:\c`, `C:\d`, `C:\e`, `C:\f`} {
		f = append(f, BinaryFinding{Path: p, SID: sidUsers, Name: "U"})
	}
	if w := ServiceBinaryWarning(`C:\a`, f); !strings.Contains(w, "and 2 more") {
		t.Errorf("warning = %s", w)
	}
}

// stoppableController is a fake service; it records every call, the service
// binary steps included, in one log.
type stoppableController struct {
	log       *[]string
	installed bool
	running   bool
	stopErr   error
	stuck     bool // keeps running after a stop request
	startErr  error
	installEr error
}

func (s *stoppableController) Install() error {
	*s.log = append(*s.log, "install")
	if s.installEr != nil {
		return s.installEr
	}
	s.installed = true
	return nil
}
func (s *stoppableController) Uninstall() error {
	*s.log = append(*s.log, "uninstall")
	s.installed = false
	return nil
}
func (s *stoppableController) Start() error {
	*s.log = append(*s.log, "start")
	if s.startErr != nil {
		return s.startErr
	}
	s.running = true
	return nil
}
func (s *stoppableController) Status() (Status, error) {
	*s.log = append(*s.log, "status")
	if !s.installed {
		return StatusUnknown, ErrNotInstalled
	}
	if s.running {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

// stop is the StopService step of this fake.
func (s *stoppableController) stop(ctx context.Context) (bool, error) {
	*s.log = append(*s.log, "stop")
	if !s.running {
		return false, nil
	}
	if s.stopErr != nil {
		return true, s.stopErr
	}
	if s.stuck {
		<-ctx.Done()
		return true, errors.New("the service did not stop in time")
	}
	s.running = false
	return true, nil
}

// installDeps are Deps whose service steps all go to fc and its log. The
// binary step copies nothing and puts "back" on a failed activation.
func installDeps(t *testing.T, fc *stoppableController) (Deps, *[]*ksvc.Config) {
	t.Helper()
	d := testDeps(t, nil)
	d.UserSID = func() (string, error) { return testSID, nil }
	cfgs := &[]*ksvc.Config{}
	d.NewControllerFromConfig = func(cfg *ksvc.Config) (Controller, error) { *cfgs = append(*cfgs, cfg); return fc, nil }
	d.StopService = fc.stop
	d.ServiceBinary = func(bi BinaryInstall, activate func(string) error) (string, error) {
		*fc.log = append(*fc.log, "copy")
		if bi.SHA256 != testSum {
			t.Errorf("copy without the verified digest: %q", bi.SHA256)
		}
		if err := activate(pfMinionExe); err != nil {
			*fc.log = append(*fc.log, "back")
			return "", err
		}
		return pfMinionExe, nil
	}
	d.Reconfigure = func(cfg *ksvc.Config, env map[string]string) (func() error, error) {
		*fc.log = append(*fc.log, "reconfigure "+cfg.Executable)
		*cfgs = append(*cfgs, cfg)
		return func() error { *fc.log = append(*fc.log, "restore"); return nil }, nil
	}
	d.SetServiceEnv = func(map[string]string) error { *fc.log = append(*fc.log, "env"); return nil }
	d.PollInterval = time.Millisecond
	return d, cfgs
}

func winReq() Request {
	return Request{Manager: ManagerWindows, Scope: ScopeSystem, Binary: userWinExe, BinarySHA256: testSum, StateDir: testState}
}

// An existing service (here an old one on the user's copy) is changed in
// place: stopped, pointed at the protected copy, started. It is never
// deleted.
func TestInstallOnWindowsChangesAnExistingServiceInPlace(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log, installed: true, running: true}
	d, cfgs := installDeps(t, fc)
	var out bytes.Buffer
	res, err := Install(context.Background(), d, winReq(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "status,copy,stop,reconfigure "+pfMinionExe+",start,status" {
		t.Errorf("calls = %s", got)
	}
	last := (*cfgs)[len(*cfgs)-1]
	if last.Executable != pfMinionExe || len(last.EnvVars) != 0 {
		t.Errorf("config %+v", last)
	}
	if res.Binary != pfMinionExe || !strings.Contains(out.String(), "verified signed release") {
		t.Errorf("result %+v, output:\n%s", res, out.String())
	}
}

func TestInstallOnWindowsCreatesAMissingService(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log}
	d, cfgs := installDeps(t, fc)
	var env map[string]string
	d.SetServiceEnv = func(e map[string]string) error { log = append(log, "env"); env = e; return nil }
	if _, err := Install(context.Background(), d, winReq(), nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "status,copy,install,env,start,status" {
		t.Errorf("calls = %s", got)
	}
	if (*cfgs)[len(*cfgs)-1].Executable != pfMinionExe {
		t.Errorf("registered %q", (*cfgs)[len(*cfgs)-1].Executable)
	}
	if env["MINION_STATE_DIR"] != testState || env[pipeUserSIDEnv] != testSID {
		t.Errorf("env = %v", env)
	}
}

// When the new binary does not start, the previous configuration and binary
// are put back and the previous service runs again: a failed reinstall never
// leaves no service.
func TestInstallOnWindowsRestoresTheServiceWhenTheNewOneDoesNotStart(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log, installed: true, running: true}
	d, _ := installDeps(t, fc)
	d.Reconfigure = func(cfg *ksvc.Config, env map[string]string) (func() error, error) {
		log = append(log, "reconfigure")
		fc.startErr = errors.New("the new binary exits at once")
		return func() error { log = append(log, "restore"); return nil }, nil
	}
	_, err := Install(context.Background(), d, winReq(), nil)
	if err == nil || !strings.Contains(err.Error(), "exits at once") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(log, ","); !strings.HasPrefix(got, "status,copy,stop,reconfigure,start,restore,back,start") {
		t.Errorf("calls = %s", got)
	}
	if strings.Contains(strings.Join(log, ","), "uninstall") {
		t.Errorf("the service was deleted: %v", log)
	}
}

func TestInstallOnWindowsRemovesAServiceItCreatedThatDoesNotStart(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log, startErr: errors.New("no")}
	d, _ := installDeps(t, fc)
	if _, err := Install(context.Background(), d, winReq(), nil); err == nil {
		t.Fatal("no error")
	}
	if got := strings.Join(log, ","); !strings.HasSuffix(got, "start,uninstall,back") {
		t.Errorf("calls = %s", got)
	}
}

func TestInstallOnWindowsStopsNothingWhenTheCopyFails(t *testing.T) {
	denyAccess(t)
	var log []string
	fc := &stoppableController{log: &log, installed: true, running: true}
	d, _ := installDeps(t, fc)
	d.ServiceBinary = func(BinaryInstall, func(string) error) (string, error) { return "", errDenied }
	_, err := Install(context.Background(), d, winReq(), nil)
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(log, ",") != "status" || !fc.running {
		t.Errorf("the service was touched: %v (running %v)", log, fc.running)
	}
}

func TestInstallOnWindowsKeepsTheServiceWhenTheStopIsDenied(t *testing.T) {
	denyAccess(t)
	var log []string
	fc := &stoppableController{log: &log, installed: true, running: true, stopErr: errDenied}
	d, _ := installDeps(t, fc)
	_, err := Install(context.Background(), d, winReq(), nil)
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(strings.Join(log, ","), "reconfigure") || !strings.Contains(strings.Join(log, ","), "back") {
		t.Errorf("calls = %v", log)
	}
}

// The stop is bounded: a service that stays in STOP_PENDING ends the
// command instead of blocking it.
func TestInstallOnWindowsBoundsTheStop(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log, installed: true, running: true, stuck: true}
	d, _ := installDeps(t, fc)
	d.StopPolls = 3
	done := make(chan error, 1)
	go func() { _, err := Install(context.Background(), d, winReq(), nil); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not stop") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the install did not end")
	}
	if strings.Contains(strings.Join(log, ","), "reconfigure") {
		t.Errorf("calls = %v", log)
	}
}

// A registry key without a service (a creation that failed half way) is
// removed before the service is created.
func TestInstallOnWindowsRemovesAnOrphanKeyBeforeCreating(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log}
	d, _ := installDeps(t, fc)
	d.ServiceKeyGone = func() bool { return false }
	d.RemoveServiceKey = func() error { log = append(log, "rmkey"); return nil }
	if _, err := Install(context.Background(), d, winReq(), nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); !strings.Contains(got, "copy,rmkey,install") {
		t.Errorf("calls = %s", got)
	}
	d.RemoveServiceKey = func() error { return errors.New("access denied") }
	fc.installed = false
	if _, err := Install(context.Background(), d, winReq(), nil); err == nil || !strings.Contains(err.Error(), "registry key") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallOnWindowsPassesTheVersionForTheDowngradeCheck(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log}
	d, _ := installDeps(t, fc)
	var got BinaryInstall
	d.ServiceBinary = func(bi BinaryInstall, activate func(string) error) (string, error) {
		got = bi
		return "", ErrServiceDowngrade
	}
	req := winReq()
	req.Version, req.AllowDowngrade = "v1.2.3", true
	if _, err := Install(context.Background(), d, req, nil); !errors.Is(err, ErrServiceDowngrade) {
		t.Fatalf("err = %v", err)
	}
	if got.Version != "v1.2.3" || !got.AllowDowngrade || got.Src != userWinExe || got.SHA256 != testSum {
		t.Errorf("binary install = %+v", got)
	}
}

// kardianos writes EnvVars into the registry before it creates the service;
// the controller of a real install gets none.
func TestWithoutEnvLeavesTheConfigOfTheRequestAlone(t *testing.T) {
	cfg := WindowsConfig(pfMinionExe, testState, testSID)
	c := withoutEnv(cfg)
	if len(c.EnvVars) != 0 || len(cfg.EnvVars) != 2 || c.Executable != cfg.Executable {
		t.Errorf("stripped %v, original %v", c.EnvVars, cfg.EnvVars)
	}
}

func TestInstallOnWindowsRefusesAnUnverifiedBinary(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log}
	d, _ := installDeps(t, fc)
	req := winReq()
	req.BinarySHA256 = ""
	_, err := Install(context.Background(), d, req, nil)
	if !errors.Is(err, ErrUnverifiedServiceBinary) {
		t.Fatalf("err = %v", err)
	}
	if len(log) != 0 {
		t.Errorf("something was done: %v", log)
	}
	// Explicitly allowed (development builds): the running binary, no digest.
	gotSum := "unset"
	d.ServiceBinary = func(bi BinaryInstall, activate func(string) error) (string, error) {
		gotSum = bi.SHA256
		return pfMinionExe, activate(pfMinionExe)
	}
	req.AllowUnverified = true
	var out bytes.Buffer
	if _, err := Install(context.Background(), d, req, &out); err != nil {
		t.Fatal(err)
	}
	if gotSum != "" || !strings.Contains(out.String(), "UNVERIFIED") {
		t.Errorf("digest = %q, output:\n%s", gotSum, out.String())
	}
}

func TestUntrustedWritersFlagsAReparsePoint(t *testing.T) {
	sd := sdProgramFiles
	sd.Reparse = true
	for _, role := range []PathRole{RoleFile, RoleDir, RoleAncestor} {
		if w := UntrustedWriters(sd, role); len(w) != 1 || w[0] != sidReparseLabel {
			t.Errorf("role %d: writers %v", role, w)
		}
	}
}

// Off Windows there is no protected binary: the guidance is the bootstrap,
// which never runs a minion.exe from the user's profile elevated.
func TestElevationGuidanceIsTheBootstrapWithoutAProtectedBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("depends on the machine's Program Files")
	}
	g := ElevationGuidance()
	if !strings.Contains(g, BootstrapCommand()) || strings.Contains(g, "minion worker up") {
		t.Errorf("guidance = %s", g)
	}
}

func TestBootstrapCommandPinsAReleaseBuild(t *testing.T) {
	old := version.Version
	t.Cleanup(func() { version.Version = old })
	version.Version = "dev"
	if c := BootstrapCommand(); !strings.Contains(c, "releases/latest/download/install.ps1") || !strings.Contains(c, "MINION_INSTALL_SERVICE='1'") {
		t.Errorf("dev: %s", c)
	}
	version.Version = "v1.2.3"
	c := BootstrapCommand()
	if !strings.Contains(c, "releases/download/v1.2.3/install.ps1") || !strings.Contains(c, "MINION_VERSION='v1.2.3'") {
		t.Errorf("release: %s", c)
	}
}
