package server

// The Worker detail page (Story 5.7) on a composed Hub: real web, API, routing and events
// modules, real HTTP and real session cookies, real SSE. Alice owns a Worker, Bob is another
// user, root is an administrator. Nothing is faked: who may see the page is decided by the
// same authorised snapshot as the lists, and a Worker outside the person's scope is the same
// plain 404 as an id that does not exist.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

func TestHubWorkerDetailWithRealSessionCookies(t *testing.T) {
	d := startDashHub(t)
	hc := d.hc
	aliceID := d.user("alice")
	d.user("bob")
	alice, _ := d.login("alice")
	bob, _ := d.login("bob")
	root, _ := d.login("root")
	wid := d.enrol("lab", aliceID, store.WorkerKindPrivate)
	link := d.connect(wid, "lab", aliceID, "private", map[string]string{"minion-1": "build <it>", "minion-2": "notes"})

	read := func(path string, hdr map[string]string) (int, string) {
		t.Helper()
		code, body, _ := hc.do("GET", path, nil, hdr)
		return code, body
	}

	// Alice: her Worker, by id, with its sessions and the one action she may take.
	code, body := read("/workers/"+wid, alice)
	if code != 200 {
		t.Fatalf("alice GET /workers/%s = %d\n%s", wid, code, body)
	}
	mustContain(t, "alice's worker page", body, `<h1>lab</h1>`, `<dt>Owner</dt><dd>alice</dd>`, `<span class="badge" data-online="true">online</span>`,
		`data-session="minion-1"`, `data-session="minion-2"`, `build &lt;it&gt;`, `data-row-link="session"`,
		`action="/w/`+wid+`/sessions"`, `sse-connect="/live/workers/`+wid+`"`, `<dt>Worker id</dt><dd><code>`+wid+`</code></dd>`)
	mustNotContain(t, "alice's worker page", body, `>Actions<`, `<th scope="col">Worker</th>`)

	// Bob: the same plain 404 as an id that does not exist; nothing of Alice's Worker.
	bc, bbody := read("/workers/"+wid, bob)
	nc, nbody := read("/workers/"+store.NewID(), bob)
	h1 := regexp.MustCompile(`<h1>(.*?)</h1>`)
	named := regexp.MustCompile(`\blab\b`) // the Worker's name as a word (not the "lab" of "label")
	if bc != 404 || nc != 404 || h1.FindString(bbody) != h1.FindString(nbody) || named.MatchString(bbody) || strings.Contains(bbody, wid) || strings.Contains(bbody, "alice") {
		t.Errorf("bob on alice's worker = %d, an unknown id = %d (%q / %q)", bc, nc, h1.FindString(bbody), h1.FindString(nbody))
	}
	// The name of a Worker never addresses it.
	if code, _ := read("/workers/lab", alice); code != 404 {
		t.Errorf("a name addressed a worker: %d", code)
	}
	// The administrator: Mine does not include Alice's Worker, All users does, with the owner.
	if code, _ := read("/workers/"+wid, root); code != 404 {
		t.Errorf("admin, Mine scope = %d, want 404", code)
	}
	code, body = read("/workers/"+wid+"?scope=all", root)
	if code != 200 || !strings.Contains(body, `<dt>Owner</dt><dd>alice</dd>`) || !strings.Contains(body, `<th scope="col">Owner</th>`) || !strings.Contains(body, `action="/w/`+wid+`/sessions"`) {
		t.Errorf("admin, All users = %d\n%s", code, body)
	}
	// A cookie that names no session: the sign-in page, nothing of the Worker.
	code, body, resp := hc.do("GET", "/workers/"+wid, nil, map[string]string{"Cookie": "minion_session=not-a-session"})
	if resp.Request.URL.Path != "/login" || named.MatchString(body) || code == 0 {
		t.Errorf("signed out ended at %s", resp.Request.URL.Path)
	}

	// The lists link each Worker by its canonical id, in the right scope.
	_, sessions := read("/sessions", alice)
	mustContain(t, "alice's session list", sessions, `<a href="/workers/`+wid+`" data-control="worker" data-worker-link>lab</a>`, `data-row-link="session"`)
	mustNotContain(t, "alice's session list", sessions, `data-control="open"`, `<th scope="col">Actions</th>`)
	_, workers := read("/workers", alice)
	mustContain(t, "alice's worker list", workers, `<th scope="row"><a href="/workers/`+wid+`" data-control="name" data-worker-link>lab</a></th>`, `data-row-link="worker"`)
	_, rootAll := read("/sessions?scope=all", root)
	mustContain(t, "root's All users list", rootAll, `<a href="/workers/`+wid+`?scope=all" data-control="worker" data-worker-link>lab</a>`)

	// Live: the page's own stream follows the Worker without a reload.
	st := d.stream("/live/workers/"+wid, alice)
	st.until("the sessions of the worker", has(`data-session="minion-1"`, `data-state="stable"`))
	link.state("minion-1", "busy", "build <it>")
	st.until("a state change", has(`data-state="busy"`))
	// Bob's stream for the same id says the worker is not there; nothing of Alice's.
	bs := d.stream("/live/workers/"+wid, bob)
	frag := bs.until("a worker that is not his", has("This worker is no longer in your list"))
	if named.MatchString(frag) || strings.Contains(frag, "minion-1") {
		t.Errorf("bob's stream leaks: %s", frag)
	}
}
