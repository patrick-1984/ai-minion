package tmux

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// The identity values of study K1, section 3, measured on tmux 3.6 with the
// format #{pid}|#{start_time}|#{session_id}|#{session_name}|#{session_created}.
const (
	k1Boot      = "a3b1c2d4-0000-4000-8000-123456789abc"
	k1Server    = "1728793"
	k1Start     = "1795615018"
	k1Server2   = "1729069" // the server after kill-server and a new start
	k1Start2    = "1795615019"
	k1BootAfter = "5e6f7a8b-1111-4111-8111-abcdef012345"
)

var instanceForm = regexp.MustCompile(`^t1-[0-9a-f]{32}$`)

// idRow is one list-panes row as tmux prints it, with the identity fields.
func idRow(created, pane, server, start, sessionID, name string) []string {
	return []string{created, "0", "1", "0", "1", "0", "1", pane, "10", "", "", "bash", server, start, sessionID, name}
}

// instanceHost is a fake host on the boot boot that prints rows.
func instanceHost(t *testing.T, boot string, rows ...[]string) (*Host, *fake) {
	t.Helper()
	f := &fake{reply: map[string]fakeReply{"list-panes": {fn: func(args []string) string {
		return codecFromArgs(args).frame(rows...)
	}}}}
	h := newFakeHost(t, f)
	h.readBoot = func() string { return boot + "\n" }
	return h, f
}

// instances lists the sessions and returns the instance id by session name.
func instances(t *testing.T, h *Host) map[string]string {
	t.Helper()
	got, err := h.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range got {
		if _, dup := out[s.Name]; dup {
			t.Fatalf("session %q listed twice", s.Name)
		}
		out[s.Name] = s.Instance
	}
	return out
}

func TestInstanceIDForm(t *testing.T) {
	got := instanceID(k1Boot, k1Server, k1Start, "$0")
	if !instanceForm.MatchString(got) {
		t.Fatalf("instance id %q does not match %s", got, instanceForm)
	}
	// SHA-256("minion-instance-v1\0tmux\0" boot "\0" pid "\0" start "\0" "$0"),
	// first 32 hex digits, computed with sha256sum: the wire form is pinned.
	if want := "t1-4b783ddf62f0f4f6e2f4d2dee3612612"; got != want {
		t.Errorf("instance id = %q, want %q", got, want)
	}
	if again := instanceID(k1Boot, k1Server, k1Start, "$0"); again != got {
		t.Errorf("not deterministic: %q then %q", got, again)
	}
}

func TestInstanceIDDiffersInEveryPart(t *testing.T) {
	base := instanceID(k1Boot, k1Server, k1Start, "$0")
	for name, other := range map[string]string{
		"boot":       instanceID(k1BootAfter, k1Server, k1Start, "$0"),
		"server pid": instanceID(k1Boot, k1Server2, k1Start, "$0"),
		"start time": instanceID(k1Boot, k1Server, k1Start2, "$0"),
		"session id": instanceID(k1Boot, k1Server, k1Start, "$1"),
		// The parts are framed: digits cannot move from one part to the next.
		"shifted digit": instanceID(k1Boot, k1Server+"1", k1Start[1:], "$0"),
	} {
		if other == "" || other == base {
			t.Errorf("another %s gives %q, base %q", name, other, base)
		}
	}
}

func TestInstanceIDEmptyWhenAPartIsMissing(t *testing.T) {
	for name, parts := range map[string][4]string{
		"no boot id":            {"", k1Server, k1Start, "$0"},
		"boot id with newline":  {k1Boot + "\n", k1Server, k1Start, "$0"},
		"boot id not hex":       {"Zebra", k1Server, k1Start, "$0"},
		"boot id too long":      {strings.Repeat("a", 65), k1Server, k1Start, "$0"},
		"no server pid":         {k1Boot, "", k1Start, "$0"},
		"server pid zero":       {k1Boot, "0", k1Start, "$0"},
		"server pid negative":   {k1Boot, "-5", k1Start, "$0"},
		"server pid not number": {k1Boot, "12x", k1Start, "$0"},
		"server pid padded":     {k1Boot, "0012", k1Start, "$0"},
		"no start time":         {k1Boot, k1Server, "", "$0"},
		"start time zero":       {k1Boot, k1Server, "0", "$0"},
		"start time not number": {k1Boot, k1Server, "yesterday", "$0"},
		"start time too long":   {k1Boot, k1Server, strings.Repeat("9", 20), "$0"},
		"no session id":         {k1Boot, k1Server, k1Start, ""},
		"session id bare":       {k1Boot, k1Server, k1Start, "0"},
		"session id only sign":  {k1Boot, k1Server, k1Start, "$"},
		"session id not number": {k1Boot, k1Server, k1Start, "$x"},
		"session id with nul":   {k1Boot, k1Server, k1Start, "$1\x00"},
		"window id":             {k1Boot, k1Server, k1Start, "@0"},
	} {
		if got := instanceID(parts[0], parts[1], parts[2], parts[3]); got != "" {
			t.Errorf("%s: instance id = %q, want none", name, got)
		}
	}
}

// TestListSessionsAsksForTheIdentityFields: the listing asks tmux for the
// server and session identity, in the framed row.
func TestListSessionsAsksForTheIdentityFields(t *testing.T) {
	h, f := instanceHost(t, k1Boot, idRow("1795615018", "%0", k1Server, k1Start, "$0", "build"))
	if _, err := h.ListSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := f.calls[len(f.calls)-1]
	format := args[len(args)-1]
	c := codecFromArgs(args)
	want := c.fs + "#{pid}" + c.fs + "#{start_time}" + c.fs + "#{session_id}" + c.fs + "#{session_name}" + c.rs
	if !strings.HasSuffix(format, want) || strings.Count(format, c.fs) != paneFields-1 {
		t.Fatalf("format = %q", format)
	}
}

// TestListSessionsReportsInstance walks the measured table of K1: a rename
// keeps the id, a name that is created again gets a new one (even in the same
// second, with the same session_created), and another server gets another id
// although tmux starts again at $0.
func TestListSessionsReportsInstance(t *testing.T) {
	const created = "1795615018"
	initial, _ := instanceHost(t, k1Boot,
		idRow(created, "%0", k1Server, k1Start, "$0", "build"),
		idRow(created, "%1", k1Server, k1Start, "$1", "other"))
	first := instances(t, initial)
	if len(first) != 2 || !instanceForm.MatchString(first["build"]) || !instanceForm.MatchString(first["other"]) {
		t.Fatalf("initial = %v", first)
	}
	if first["build"] == first["other"] {
		t.Fatalf("two sessions of one server share the id %q", first["build"])
	}
	if first["build"] != instanceID(k1Boot, k1Server, k1Start, "$0") {
		t.Errorf("build = %q, not the id of its identity fields", first["build"])
	}

	// rename-session build renamed: same $0.
	renamed, _ := instanceHost(t, k1Boot,
		idRow(created, "%0", k1Server, k1Start, "$0", "renamed"),
		idRow(created, "%1", k1Server, k1Start, "$1", "other"))
	if got := instances(t, renamed); got["renamed"] != first["build"] || got["other"] != first["other"] {
		t.Errorf("after the rename = %v, want the ids of %v", got, first)
	}

	// A new session takes the old name "build" while the first one lives on
	// as "renamed": $2.
	reused, _ := instanceHost(t, k1Boot,
		idRow(created, "%0", k1Server, k1Start, "$0", "renamed"),
		idRow(created, "%2", k1Server, k1Start, "$2", "build"))
	second := instances(t, reused)
	if second["build"] == "" || second["build"] == first["build"] || second["renamed"] != first["build"] {
		t.Errorf("after the name was reused = %v, first %v", second, first)
	}

	// kill-session build and new-session build inside the same second: the
	// creation time is the same, the id is not ($3).
	recreated, _ := instanceHost(t, k1Boot, idRow(created, "%3", k1Server, k1Start, "$3", "build"))
	third := instances(t, recreated)
	if third["build"] == "" || third["build"] == first["build"] || third["build"] == second["build"] {
		t.Errorf("re-created in the same second = %v, earlier %v and %v", third, first, second)
	}

	// The server was killed and started again: $0 and the name are back.
	restarted, _ := instanceHost(t, k1Boot, idRow("1795615019", "%0", k1Server2, k1Start2, "$0", "build"))
	fourth := instances(t, restarted)
	if fourth["build"] == "" || fourth["build"] == first["build"] {
		t.Errorf("another server = %v, first %v", fourth, first)
	}

	// The same pid, start time and $0 on another boot (a machine without a
	// clock starts at the same time after every boot).
	rebooted, _ := instanceHost(t, k1BootAfter, idRow(created, "%0", k1Server, k1Start, "$0", "build"))
	if got := instances(t, rebooted); got["build"] == "" || got["build"] == first["build"] {
		t.Errorf("another boot = %v, first %v", got, first)
	}

	// A Worker restart is a new Host over the same server: the same id.
	again, _ := instanceHost(t, k1Boot, idRow(created, "%0", k1Server, k1Start, "$0", "build"))
	if got := instances(t, again); got["build"] != first["build"] {
		t.Errorf("a new host over the same server = %v, first %v", got, first)
	}
}

// TestListSessionsOneInstancePerSession: the panes of a session are one
// session with one id, and a name cannot forge identity fields.
func TestListSessionsOneInstancePerSession(t *testing.T) {
	h, _ := instanceHost(t, k1Boot,
		idRow("1", "%0", k1Server, k1Start, "$0", "wb-1"),
		idRow("1", "%1", k1Server, k1Start, "$0", "wb-1"),
		idRow("1", "%2", k1Server, k1Start, "$1", "$0"))
	got, err := h.ListSessions(context.Background())
	if err != nil || len(got) != 2 || len(got[0].Panes) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].Instance != instanceID(k1Boot, k1Server, k1Start, "$0") ||
		got[1].Instance != instanceID(k1Boot, k1Server, k1Start, "$1") || got[1].Name != "$0" {
		t.Errorf("sessions = %+v", got)
	}
	if !h.ReportsInstances() {
		t.Error("a host that identified every session does not report instances")
	}
}

// TestListSessionsWithoutIdentityHasNoInstance: where a part cannot be read
// there is no id, and the host stops claiming that it reports instances.
func TestListSessionsWithoutIdentityHasNoInstance(t *testing.T) {
	// A tmux that does not know #{start_time} prints nothing for it.
	old, _ := instanceHost(t, k1Boot, idRow("1", "%0", k1Server, "", "$0", "build"))
	if !old.ReportsInstances() {
		t.Error("before the first listing a tmux host with a boot id is taken to report instances")
	}
	if got := instances(t, old); got["build"] != "" {
		t.Errorf("without a start time: %v", got)
	}
	if old.ReportsInstances() {
		t.Error("the host still reports instances after a listing without ids")
	}

	// The boot id cannot be read (another system, a restricted /proc).
	for _, boot := range []string{"", "not a boot id"} {
		noBoot, _ := instanceHost(t, boot, idRow("1", "%0", k1Server, k1Start, "$0", "build"))
		if noBoot.ReportsInstances() {
			t.Errorf("boot id %q: the host reports instances", boot)
		}
		if got := instances(t, noBoot); got["build"] != "" {
			t.Errorf("boot id %q: %v", boot, got)
		}
	}

	// A fixture without identity fields at all (the rows of the other tests).
	plain, _ := instanceHost(t, k1Boot, row("%0", "", "bash", "build"))
	if got := instances(t, plain); got["build"] != "" {
		t.Errorf("without identity fields: %v", got)
	}
}

// TestInstanceGapHealsWithTheNextListing: the claim follows the latest
// listing, so a host is not marked for good by one server.
func TestInstanceGapHealsWithTheNextListing(t *testing.T) {
	rows := [][]string{idRow("1", "%0", k1Server, "", "$0", "build")}
	f := &fake{reply: map[string]fakeReply{"list-panes": {fn: func(args []string) string {
		return codecFromArgs(args).frame(rows...)
	}}}}
	h := newFakeHost(t, f)
	reads := 0
	h.readBoot = func() string { reads++; return k1Boot }
	if _, err := h.ListSessions(context.Background()); err != nil || h.ReportsInstances() {
		t.Fatalf("after a listing without ids: reports = %v, err = %v", h.ReportsInstances(), err)
	}
	rows = [][]string{idRow("1", "%0", k1Server2, k1Start2, "$0", "build")}
	if _, err := h.ListSessions(context.Background()); err != nil || !h.ReportsInstances() {
		t.Fatalf("after a listing with ids: reports = %v, err = %v", h.ReportsInstances(), err)
	}
	if reads != 1 {
		t.Errorf("the boot id was read %d times, want once", reads)
	}
}

// TestUsageListingKeepsTheHostID: the usage listing still gets the native
// session id in HostID, next to the instance.
func TestUsageListingKeepsTheHostID(t *testing.T) {
	// In the usage listing tmux prints #{session_id} in the task field too.
	r := idRow("1", "%0", k1Server, k1Start, "$4", "build")
	r[9], r[10] = "$4", "/srv/app"
	h, f := instanceHost(t, k1Boot, r)
	got, err := h.ListUsageSessions(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].HostID != "$4" || got[0].Panes[0].Folder != "/srv/app" || got[0].Task != "" ||
		got[0].Instance != instanceID(k1Boot, k1Server, k1Start, "$4") {
		t.Errorf("usage session = %+v", got[0])
	}
	args := f.calls[len(f.calls)-1]
	if n := strings.Count(args[len(args)-1], "#{session_id}"); n != 2 {
		t.Errorf("the usage format names #{session_id} %d times, want 2", n)
	}
}

// TestPsmuxIsNotAskedForIdentity: psmux gets no format variable it did not get
// before, its sessions have no instance and the host does not claim any.
func TestPsmuxIsNotAskedForIdentity(t *testing.T) {
	f := &fakePsmux{}
	h, err := New("", WithPsmux(), WithExec(f.exec))
	if err != nil {
		t.Fatal(err)
	}
	h.readBoot = func() string { return k1Boot }
	ctx := context.Background()
	got, err := h.ListSessions(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, s := range got {
		if s.Instance != "" {
			t.Errorf("psmux session %q has the instance %q", s.Name, s.Instance)
		}
	}
	format := f.calls[len(f.calls)-1][len(f.calls[len(f.calls)-1])-1]
	for _, v := range []string{"#{pid}", "#{start_time}", "#{session_id}"} {
		if strings.Contains(format, v) {
			t.Errorf("psmux was asked for %s: %q", v, format)
		}
	}
	if _, err := h.ListUsageSessions(ctx); err != nil {
		t.Fatal(err)
	}
	usage := f.calls[len(f.calls)-1][len(f.calls[len(f.calls)-1])-1]
	if strings.Count(usage, "#{session_id}") != 1 || strings.Contains(usage, "#{pid}") || strings.Contains(usage, "#{start_time}") {
		t.Errorf("psmux usage format = %q", usage)
	}
	if h.ReportsInstances() {
		t.Error("a psmux host reports instances")
	}
	if term.ReportsInstances(h) {
		t.Error("term.ReportsInstances is true for psmux")
	}
}

// TestTmuxHostIsAnInstanceReporter: the port the Worker reads to announce the
// feature.
func TestTmuxHostIsAnInstanceReporter(t *testing.T) {
	h, _ := instanceHost(t, k1Boot, idRow("1", "%0", k1Server, k1Start, "$0", "build"))
	if !term.ReportsInstances(h) {
		t.Error("a tmux host with a boot id is not an instance reporter")
	}
}
