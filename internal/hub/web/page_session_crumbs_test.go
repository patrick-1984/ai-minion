package web

// Tests of the breadcrumb and the quick switcher of the full session page (A117).

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const crumbOtherWorkerID = "0190a1b2-c3d4-7e5f-8a6b-0000000000b2"

// crumbSource is a live snapshot with the session of the page (with a code), a
// session with a hostile name on the same Worker, a session of another Worker
// and two Workers, one with a hostile name.
func crumbSource(t *testing.T) *fakeSource {
	t.Helper()
	d := Dashboard{
		Workers: []WorkerRow{
			{ID: sessWorkerID, Name: "pi-1", Owner: "bob", Online: true},
			{ID: crumbOtherWorkerID, Name: `<i>lab</i>`, Owner: "bob", Online: false},
		},
		Sessions: []SessionRow{
			{WorkerID: sessWorkerID, Worker: "pi-1", Name: "minion-1", Code: "5-667", State: ops.PaneStateStable, Online: true},
			{WorkerID: sessWorkerID, Worker: "pi-1", Name: `x"><b>`, State: ops.PaneStateDialog, Online: true},
			{WorkerID: crumbOtherWorkerID, Worker: "lab", Name: "elsewhere", Online: false},
		},
	}
	src := newFakeSource(d, d)
	SetLiveSource(src)
	t.Cleanup(func() { SetLiveSource(nil) })
	return src
}

func TestSessionPageBreadcrumbLinksAndSwitcher(t *testing.T) {
	s, _ := sessionOps(t)
	src := crumbSource(t)
	rec := sessGet(t, s, "/s/pi-1/minion-1", sessUser)
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	btn := func(label string) string {
		return `<button type="button" class="crumb-switch-btn" popovertarget="crumb-switch" aria-label="Switch from ` + label +
			` to another session or Worker" title="Switch session or Worker">`
	}
	mustHave(t, body,
		// Control center / Workers / pi-1 / 5-667 minion-1: every part a link, the
		// Worker and the session open the switcher, the code comes first.
		`<li><a href="/">Control center</a></li>`, `<li><a href="/workers">Workers</a></li>`,
		`<li><a href="/workers/`+sessWorkerID+`"><span class="crumb-label" title="pi-1">pi-1</span></a>`+btn("pi-1"),
		`<li><a href="/s/pi-1/minion-1" aria-current="page"><code class="crumb-code">5-667</code> <span class="crumb-label" title="minion-1">minion-1</span></a>`+btn("minion-1"),
		// The switcher: a native popover with the Worker's sessions and the Workers.
		`<div class="info-pop crumb-switch" id="crumb-switch" popover data-crumb-switch>`,
		`<p class="crumb-switch-h" id="crumb-switch-s">Sessions on pi-1</p>`,
		`<a href="/s/pi-1/minion-1" data-filter="5-667 minion-1" aria-current="page"><code class="crumb-code">5-667</code> <span class="crumb-switch-name">minion-1</span><span class="crumb-switch-meta">Idle</span></a>`,
		// A hostile session name is escaped in the text, the filter and the path.
		`<a href="/s/pi-1/x%22%3E%3Cb%3E" data-filter=" x&#34;&gt;&lt;b&gt;"><span class="crumb-switch-name">x&#34;&gt;&lt;b&gt;</span>`,
		`<a href="/workers/`+sessWorkerID+`" data-filter="pi-1" aria-current="true">`,
		`<a href="/workers/`+crumbOtherWorkerID+`" data-filter="&lt;i&gt;lab&lt;/i&gt;"><span class="crumb-switch-name">&lt;i&gt;lab&lt;/i&gt;</span><span class="crumb-switch-meta">offline</span></a>`)
	// Only this Worker's sessions are offered; nothing hostile is unescaped.
	mustNotHave(t, body, `elsewhere`, `<i>lab</i>`, `x"><b>`, `<li><a href="/sessions">Sessions</a></li>`)
	// A user's own scope: the snapshot is not widened.
	for _, all := range src.askedAll {
		if all {
			t.Errorf("a user's switcher asked for every user's snapshot")
		}
	}
}

func TestSessionPageBreadcrumbWithoutLiveListsOrEmbedded(t *testing.T) {
	s, _ := sessionOps(t)
	// No live source: the breadcrumb is still complete, without a switcher.
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustHave(t, body, `<li><a href="/workers/`+sessWorkerID+`"><span class="crumb-label" title="pi-1">pi-1</span></a></li>`, `<li><a href="/s/pi-1/minion-1" aria-current="page"><span class="crumb-label" title="minion-1">minion-1</span></a></li>`)
	mustNotHave(t, body, `popovertarget="crumb-switch"`, `data-crumb-switch`, `crumb-code`)

	// The compact view inside the inspector has no topbar and no switcher.
	src := crumbSource(t)
	embed := sessGet(t, s, "/s/pi-1/minion-1?embed=1", sessUser).Body.String()
	mustNotHave(t, embed, `data-crumb-switch`, `crumb-switch-btn`)
	if len(src.askedAll) != 0 {
		t.Errorf("the embedded view read the live snapshot %d times", len(src.askedAll))
	}
}

func TestSessionPageBreadcrumbOfAnAdminOnAnotherUsersWorker(t *testing.T) {
	s, f := sessionOps(t)
	foreign := sessionWorker()
	foreign.OwnerID, foreign.Owner = "u-carol", "carol"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{foreign}})
	all := Dashboard{
		Workers: []WorkerRow{
			{ID: sessWorkerID, Name: "pi-1", Owner: "carol", Online: true},
			{ID: "w-alice", Name: "alice-box", Owner: "alice", Online: true},
			{ID: "w-dave", Name: "dave-box", Owner: "dave", Online: true},
		},
		Sessions: []SessionRow{{WorkerID: sessWorkerID, Worker: "pi-1", Name: "minion-1", Online: true}},
	}
	src := newFakeSource(Dashboard{}, all)
	SetLiveSource(src)
	t.Cleanup(func() { SetLiveSource(nil) })
	body := sessGet(t, s, "/s/pi-1/minion-1", adminHeader).Body.String()
	// The links keep the All users scope where another owner's Worker needs it.
	mustHave(t, body, `<li><a href="/workers?scope=all">Workers</a></li>`, `<a href="/workers/`+sessWorkerID+`?scope=all"><span class="crumb-label" title="pi-1">pi-1</span></a>`,
		`<a href="/workers/w-alice" data-filter="alice-box">`)
	// The switcher offers the admin's own Workers and this one, not the whole Hub.
	if strings.Contains(body, "dave-box") {
		t.Error("the switcher of an admin lists another user's Worker")
	}
	if len(src.askedAll) != 1 || !src.askedAll[0] {
		t.Errorf("snapshot scopes = %v, want [true]", src.askedAll)
	}
}
