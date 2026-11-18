package service

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	ksvc "github.com/kardianos/service"
)

const (
	userSID      = "S-1-5-21-1004336348-1177238915-682003330-1001"
	pfMinionDir  = `C:\Program Files\Minion`
	pfMinionExe  = `C:\Program Files\Minion\minion.exe`
	userWinExe   = `C:\Users\alice\AppData\Local\Programs\minion\minion.exe`
	fullControl  = 0x1f01ff
	readExecute  = 0x1200a9
	modifyRights = 0x1301bf
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
	for _, want := range []string{"writable by non-administrators", pfMinionExe, userSID, `BUILTIN\Users`, "minion worker up", "elevated", "LocalSystem"} {
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

// stoppableController is a fake whose service runs until it is stopped; it
// records every call, the service binary steps included, in one log.
type stoppableController struct {
	log     *[]string
	running bool
	stopErr error
	// deleted is set between Uninstall and Install; lingering is how many
	// Status calls still see the deleted service (marked for deletion).
	deleted   bool
	lingering int
}

func (s *stoppableController) Install() error {
	*s.log = append(*s.log, "install")
	s.deleted = false
	return nil
}
func (s *stoppableController) Uninstall() error {
	*s.log = append(*s.log, "uninstall")
	s.deleted = true
	return nil
}
func (s *stoppableController) Start() error {
	*s.log = append(*s.log, "start")
	s.running = true
	return nil
}
func (s *stoppableController) Stop() error {
	*s.log = append(*s.log, "stop")
	if s.stopErr != nil {
		return s.stopErr
	}
	s.running = false
	return nil
}
func (s *stoppableController) Status() (Status, error) {
	if s.deleted {
		if s.lingering == 0 {
			*s.log = append(*s.log, "gone")
			return StatusUnknown, ErrNotInstalled
		}
		s.lingering--
	}
	*s.log = append(*s.log, "status")
	if s.running {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

func TestInstallOnWindowsRegistersTheProtectedCopy(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log, running: true}
	d := testDeps(t, nil)
	d.UserSID = func() (string, error) { return testSID, nil }
	var cfgs []*ksvc.Config
	d.NewControllerFromConfig = func(cfg *ksvc.Config) (Controller, error) { cfgs = append(cfgs, cfg); return fc, nil }
	var copied string
	d.ServiceBinary = func(src string) (string, error) {
		log = append(log, "copy")
		copied = src
		return pfMinionExe, nil
	}
	var out bytes.Buffer
	res, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Scope: ScopeSystem, Binary: userWinExe, StateDir: testState}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if copied != userWinExe {
		t.Errorf("copied %q", copied)
	}
	// The running service (of an older install) is stopped before its binary
	// is replaced, and the new service is registered with the copy.
	if got := strings.Join(log, ","); !strings.HasPrefix(got, "status,stop,status,copy,uninstall,gone,install,start,status") {
		t.Errorf("calls = %s", got)
	}
	last := cfgs[len(cfgs)-1]
	if last.Executable != pfMinionExe {
		t.Errorf("registered %q", last.Executable)
	}
	if res.Binary != pfMinionExe || !strings.Contains(out.String(), pfMinionExe) || !strings.Contains(out.String(), "only administrators") {
		t.Errorf("result %+v, output:\n%s", res, out.String())
	}
}

func TestInstallOnWindowsDoesNotStopAStoppedService(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log}
	d := testDeps(t, nil)
	d.UserSID = func() (string, error) { return testSID, nil }
	d.NewControllerFromConfig = func(*ksvc.Config) (Controller, error) { return fc, nil }
	if _, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: userWinExe}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(log, ","), "stop") {
		t.Errorf("calls = %v", log)
	}
}

func TestInstallOnWindowsStopsNothingWhenTheStopIsDenied(t *testing.T) {
	denyAccess(t)
	var log []string
	fc := &stoppableController{log: &log, running: true, stopErr: errDenied}
	d := testDeps(t, nil)
	d.UserSID = func() (string, error) { return testSID, nil }
	d.NewControllerFromConfig = func(*ksvc.Config) (Controller, error) { return fc, nil }
	d.ServiceBinary = func(string) (string, error) { t.Fatal("binary copied after a failed stop"); return "", nil }
	_, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: userWinExe}, nil)
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallOnWindowsStopsWhenTheServiceDoesNotStop(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log, running: true}
	d := testDeps(t, nil)
	d.UserSID = func() (string, error) { return testSID, nil }
	d.StopPolls = 2
	d.NewControllerFromConfig = func(*ksvc.Config) (Controller, error) { return stuck{fc}, nil }
	_, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: userWinExe}, nil)
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("err = %v", err)
	}
}

// stuck is a service that keeps running after Stop.
type stuck struct{ *stoppableController }

func (s stuck) Stop() error { return nil }

func TestInstallOnWindowsFailsWhenTheCopyFails(t *testing.T) {
	denyAccess(t)
	fc := &fakeController{}
	d, fx := windowsDeps(t, fc)
	d.ServiceBinary = func(string) (string, error) { return "", errDenied }
	_, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: userWinExe}, nil)
	if err == nil || !strings.Contains(err.Error(), "prepare the binary of the service") || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("err = %v", err)
	}
	if len(fx.fc.calls) != 0 {
		t.Errorf("the service was touched: %v", fx.fc.calls)
	}
}

// DeleteService only marks the service for deletion: the install waits until
// it is gone before it creates it again (else the new service's registry key,
// with its environment, can be removed with the old one).
func TestInstallOnWindowsWaitsUntilTheOldServiceIsGone(t *testing.T) {
	var log []string
	fc := &stoppableController{log: &log, lingering: 2}
	d := testDeps(t, nil)
	d.UserSID = func() (string, error) { return testSID, nil }
	d.NewControllerFromConfig = func(*ksvc.Config) (Controller, error) { return fc, nil }
	if _, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: userWinExe}, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); !strings.Contains(got, "uninstall,status,status,gone,install") {
		t.Errorf("calls = %s", got)
	}

	log = nil
	fc = &stoppableController{log: &log, lingering: 1000}
	d.StopPolls = 3
	_, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: userWinExe}, nil)
	if err == nil || !strings.Contains(err.Error(), "still being deleted") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range log {
		if c == "install" {
			t.Errorf("the service was created while the old one was still being deleted: %v", log)
		}
	}
}
