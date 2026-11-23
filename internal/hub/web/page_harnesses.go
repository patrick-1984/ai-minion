package web

// The Harnesses page (A69): which coding harnesses (Claude Code, Codex, ...) are
// installed on the user's Workers, compared with the list the user ticked.
//
// It adds no collection of its own. The tick list is stored per user in the
// account preferences (account_prefs / account_prefs_set), the scan is the
// existing Worker inventory operation (worker_usage with inventory) and the
// matrix reads the stored inventory of usage_list. The harnesses are exactly
// those the Worker detects (internal/worker/agent/usage.go); DeepSeek has no
// official CLI, so the page only says how to use it.

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const harnessesPath = "/harnesses"

// knownHarness is one harness the Worker inventory detects.
type knownHarness struct{ Key, Label string }

type harnessVendor struct {
	Vendor string
	Items  []harnessChoice
	Note   string
}

type harnessChoice struct {
	Key, Label string
	Checked    bool
}

// harnessVendors is the order of the checklist. Keys are the inventory names.
var harnessVendors = []struct {
	Vendor string
	Items  []knownHarness
	Note   string
}{
	{"Anthropic", []knownHarness{{"claude", "Claude Code"}}, ""},
	{"OpenAI", []knownHarness{{"codex", "Codex"}}, ""},
	{"Google", []knownHarness{{"gemini", "Gemini CLI"}}, ""},
	{"Alibaba", []knownHarness{{"qwen", "Qwen Code"}}, ""},
	{"Open source", []knownHarness{{"opencode", "OpenCode"}, {"aider", "Aider"}, {"cline", "Cline"}}, ""},
	{"DeepSeek", nil, "DeepSeek has no official CLI. Use its models through OpenCode or Aider."},
}

func harnessLabel(key string) string {
	for _, v := range harnessVendors {
		for _, h := range v.Items {
			if h.Key == key {
				return h.Label
			}
		}
	}
	return usageLabel(key)
}

type harnessCell struct {
	Text, Tone string
	Missing    bool
}

type harnessRow struct {
	ID, Name, Owner string
	Online          bool
	Scanned         string
	Cells           []harnessCell
	Missing         bool
}

type harnessPage struct {
	Scope, MineURL, AllURL, RefreshURL, ScanURL, ChooseURL, ClearURL, UsageURL string
	ForUser                                                                    string
	CanFilter, ShowOwner, OnlyMissing, Private, HasWorkers                     bool
	Vendors                                                                    []harnessVendor
	Columns                                                                    []string
	Rows                                                                       []harnessRow
	Total                                                                      int
}

func harnessQuery(all, missing bool) string {
	q := url.Values{}
	if all {
		q.Set("scope", scopeAll)
	}
	if missing {
		q.Set("missing", "1")
	}
	if len(q) == 0 {
		return harnessesPath
	}
	return harnessesPath + "?" + q.Encode()
}

// harnessCellFor judges one Worker and one harness from the stored inventory.
func harnessCellFor(w ops.WorkerInfo, row *ops.UsageWorkerRow, key string) harnessCell {
	unknown := func(why string) harnessCell { return harnessCell{Text: "Unknown (" + why + ")", Tone: "info"} }
	if row == nil || row.InventoryAt == nil {
		switch {
		case !w.Online:
			return unknown("offline, not scanned")
		case row != nil && row.Status == "unsupported":
			return unknown("Worker update needed")
		case row != nil && row.Status == "registration_required":
			return unknown("monitoring not accepted")
		}
		return unknown("not scanned")
	}
	for _, h := range row.Inventory {
		if h.Name != key {
			continue
		}
		switch h.Status {
		case "installed":
			text := "Installed"
			if h.Version != "" {
				text += " v" + h.Version
			}
			return harnessCell{Text: text, Tone: "positive"}
		case "unsupported":
			return unknown("not supported by this Worker")
		}
		return harnessCell{Text: "Missing", Tone: "warning", Missing: true}
	}
	return unknown("not in this scan")
}

func harnessesScope(r *http.Request) (all, canFilter bool) { return dashboardScope(r) }

// harnessWorkers lists the Workers of the scope that are approved.
func harnessWorkers(r *http.Request, c OpCaller, all bool) ([]ops.WorkerInfo, error) {
	var list ops.WorkersListResult
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{All: all}, &list); err != nil {
		return nil, err
	}
	out := make([]ops.WorkerInfo, 0, len(list.Workers))
	for _, w := range list.Workers {
		if !w.Pending {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b ops.WorkerInfo) int {
		if a.Name != b.Name {
			if a.Name < b.Name {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	return out, nil
}

// harnessInventory reads the stored inventory of every visible Worker, page by page.
func harnessInventory(r *http.Request, c OpCaller) (map[string]*ops.UsageWorkerRow, error) {
	rows := map[string]*ops.UsageWorkerRow{}
	after := ""
	for range 20 {
		var res ops.UsageListResult
		if err := c.CallOp(r, ops.OpUsageList, ops.UsageListArgs{Section: "workers", After: after, Limit: 200}, &res); err != nil {
			return nil, err
		}
		for i := range res.Workers {
			rows[res.Workers[i].ID] = &res.Workers[i]
		}
		if res.Next == "" {
			break
		}
		after = res.Next
	}
	return rows, nil
}

func harnessesPageData(r *http.Request, c OpCaller, u pageUser) (any, error) {
	all, canFilter := harnessesScope(r)
	missing := r.URL.Query().Get("missing") == "1"
	p := harnessPage{
		Scope: scopeMine, ForUser: u.ID, CanFilter: canFilter, ShowOwner: all, OnlyMissing: missing, Private: usagePrivate(r),
		MineURL: harnessQuery(false, missing), AllURL: harnessQuery(true, missing), RefreshURL: harnessQuery(all, missing),
		ScanURL: harnessesPath + "/scan" + harnessQuery(all, false)[len(harnessesPath):], ClearURL: harnessQuery(all, false),
		ChooseURL: harnessesPath + "/choose" + harnessQuery(all, missing)[len(harnessesPath):], UsageURL: usagePath + "?view=workers",
	}
	if all {
		p.Scope = scopeAll
	}
	var prefs ops.AccountPrefsResult
	if err := c.CallOp(r, ops.OpAccountPrefs, ops.AccountPrefsArgs{}, &prefs); err != nil {
		return nil, err
	}
	chosen := func(key string) bool {
		return prefs.WantedHarnesses == nil || slices.Contains(prefs.WantedHarnesses, key)
	}
	var cols []string
	for _, v := range harnessVendors {
		hv := harnessVendor{Vendor: v.Vendor, Note: v.Note}
		for _, h := range v.Items {
			on := chosen(h.Key)
			hv.Items = append(hv.Items, harnessChoice{Key: h.Key, Label: h.Label, Checked: on})
			if on {
				cols = append(cols, h.Key)
			}
		}
		p.Vendors = append(p.Vendors, hv)
	}
	for _, k := range cols {
		p.Columns = append(p.Columns, harnessLabel(k))
	}
	workers, err := harnessWorkers(r, c, all)
	if err != nil {
		return nil, err
	}
	inv, err := harnessInventory(r, c)
	if err != nil {
		return nil, err
	}
	p.Total = len(workers)
	for _, w := range workers {
		row := harnessRow{ID: w.ID, Name: w.Name, Owner: w.Owner, Online: w.Online, Scanned: "Not scanned"}
		ur := inv[w.ID]
		if ur != nil && ur.InventoryAt != nil {
			row.Scanned = usagePage{}.Time(ur.InventoryAt)
		}
		for _, k := range cols {
			cell := harnessCellFor(w, ur, k)
			row.Missing = row.Missing || cell.Missing
			row.Cells = append(row.Cells, cell)
		}
		if missing && !row.Missing {
			continue
		}
		p.Rows = append(p.Rows, row)
	}
	p.HasWorkers = p.Total > 0
	return p, nil
}

func harnessesPost(w http.ResponseWriter, r *http.Request, c OpCaller, u pageUser) {
	if err := r.ParseForm(); err != nil {
		writeResultError(w, "The form could not be read.")
		return
	}
	if r.PostFormValue("for_user") != u.ID {
		writeResultError(w, "The signed-in account changed. Reload the Harnesses page.")
		return
	}
	switch r.PathValue("action") {
	case "choose":
		if !usagePrivate(r) {
			writeResultError(w, "The harness choice is saved in your own signed-in account.")
			return
		}
		names := []string{}
		for _, v := range harnessVendors {
			for _, h := range v.Items {
				if slices.Contains(r.PostForm["harness"], h.Key) {
					names = append(names, h.Key)
				}
			}
		}
		writeOp(w, r, c, ops.OpAccountPrefsSet, ops.AccountPrefsSetArgs{WriteArgs: newWriteArgs(), WantedHarnesses: &names}, "Your harness choice is saved.")
	case "scan":
		all, _ := harnessesScope(r)
		workers, err := harnessWorkers(r, c, all)
		if err != nil {
			writeResultError(w, failureText(err))
			return
		}
		inv, err := harnessInventory(r, c)
		if err != nil {
			writeResultError(w, failureText(err))
			return
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		failed, skipped := 0, 0
		replied := map[string]int64{}
		// A Worker counts as checked only when the Hub recorded an inventory
		// time newer than the one it already had: a cached reply or a scan
		// that the store discarded is not a check.
		before := func(id string) int64 {
			if row := inv[id]; row != nil && row.InventoryAt != nil {
				return *row.InventoryAt
			}
			return 0
		}
		for _, wk := range workers {
			row := inv[wk.ID]
			if !wk.Online || !slices.Contains(wk.Ops, ops.OpWorkerUsage) || row != nil && row.Status == "registration_required" {
				skipped++
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			was := before(wk.ID)
			go func() {
				defer func() { <-sem; wg.Done() }()
				var report ops.UsageReport
				err := c.CallOp(r, ops.OpWorkerUsage, ops.WorkerUsageArgs{WorkerID: wk.ID, Inventory: true}, &report)
				mu.Lock()
				defer mu.Unlock()
				if err != nil || report.InventoryAt == nil || *report.InventoryAt <= was {
					failed++
				} else {
					replied[wk.ID] = *report.InventoryAt
				}
			}()
		}
		wg.Wait()
		ok := 0
		if len(replied) > 0 {
			recorded, err := harnessInventory(r, c)
			if err != nil {
				writeResultError(w, failureText(err))
				return
			}
			for id, at := range replied {
				if row := recorded[id]; row != nil && row.InventoryAt != nil && *row.InventoryAt >= at {
					ok++
				} else {
					failed++
				}
			}
		}
		text := "Checked " + pluralWorkers(ok) + "."
		if failed > 0 {
			text += " " + pluralWorkers(failed) + " could not be checked or returned a scan that is not newer (timed out or cached)."
		}
		if skipped > 0 {
			text += " " + pluralWorkers(skipped) + " skipped (offline, needs an update or has not accepted monitoring)."
		}
		done(w, r, text, ops.WriteResult{})
	default:
		http.NotFound(w, r)
	}
}

func pluralWorkers(n int) string {
	if n == 1 {
		return "1 Worker"
	}
	return strconv.Itoa(n) + " Workers"
}

func init() {
	RegisterPage(Page{Pattern: harnessesPath, Title: "Harnesses", Template: "harnesses.html", Data: userPageData(harnessesPageData)})
	RegisterNav(NavItem{Label: "Harnesses", Path: harnessesPath, Order: 126, Group: NavGroupWorkspace, Icon: "i-monitor"})
	RegisterRoute(Route{Pattern: "POST " + harnessesPath + "/{action}", Handler: userRoute(harnessesPost)})
}
