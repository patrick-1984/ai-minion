package web

// The file transfer history (files.v1, BRD minion-file-transfer FR-17, 5.9,
// WP9): one page of file_transfers_list as the signed-in person, newest
// first. The operation decides what the person sees: the transfers that touch
// a Worker they own or that they started, every transfer for an
// administrator. A transfer id selects one transfer with its entries. The
// page shows display roots and paths relative to them, never an absolute
// path, a token or content; only the cursor and the transfer id go in a URL.

import (
	"net/http"
	"net/url"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathFilesHistory = "/files/history"

// FilesHistoryPage is what files_history.html sees.
type FilesHistoryPage struct {
	Rows []FilesHistoryRow
	// Next is the link to the next page; empty on the last page.
	Next string
	// One is set when one transfer is selected; its entries are listed.
	One     *FilesHistoryRow
	Entries []FilesHistoryEntry
	Omitted int64
	Error   string
}

// FilesHistoryRow is one transfer.
type FilesHistoryRow struct {
	ID        string
	Link      string
	When      string
	WhenAt    string
	Direction string
	State     string
	Tone      string
	Reason    string
	Who       string
	Target    string
	Source    string
	Root      string
	Path      string
	Kind      string
	Files     int64
	Bytes     string
	Manifest  string
}

// FilesHistoryEntry is one file of a selected transfer.
type FilesHistoryEntry struct {
	Path    string
	Size    string
	SHA256  string
	Skipped string
}

// stateTone is the badge tone of a transfer state.
func stateTone(state string) string {
	switch state {
	case ops.FileStateDelivered, ops.FileStateServed:
		return "positive"
	case ops.FileStatePartial, ops.FileStateUnknown, ops.FileStateAdmitted, ops.FileStatePrepared:
		return "warning"
	}
	return "danger"
}

func historyRowOf(t ops.FileTransferRecord, names map[string]string) FilesHistoryRow {
	name := func(id string) string {
		if n := names[id]; n != "" {
			return n
		}
		return id
	}
	row := FilesHistoryRow{
		ID: t.TransferID, Link: pathFilesHistory + "?transfer=" + url.QueryEscape(t.TransferID),
		Direction: t.Direction, State: t.State, Tone: stateTone(t.State), Reason: t.Reason,
		Who: t.Principal, Root: t.TargetRoot, Path: t.Path, Kind: t.Kind, Files: t.Files,
		Bytes: formatBytes(t.Bytes), Manifest: t.ManifestDigest,
		Target: t.TargetSession + " on " + name(t.TargetWorkerID),
	}
	if t.ActingAs != "" {
		row.Who += " (acting as " + t.ActingAs + ")"
	}
	if t.SourceWorkerID != "" {
		row.Source = t.SourceSession + " on " + name(t.SourceWorkerID)
	} else {
		row.Source = "web or REST client"
	}
	if at := time.Time(t.CreatedAt); !at.IsZero() {
		row.When, row.WhenAt = at.UTC().Format("2006-01-02 15:04:05"), at.UTC().Format(time.RFC3339)
	}
	return row
}

// workerNames maps Worker ids to names, as far as the viewer can see them.
func workerNames(r *http.Request, c OpCaller, u pageUser) map[string]string {
	var res ops.WorkersListResult
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{All: u.Admin}, &res); err != nil {
		return nil
	}
	out := make(map[string]string, len(res.Workers))
	for _, w := range res.Workers {
		out[w.ID] = w.Name
	}
	return out
}

func filesHistoryData(r *http.Request, c OpCaller, u pageUser) (any, error) {
	q := r.URL.Query()
	args := ops.FileTransfersListArgs{Cursor: q.Get("cursor"), TransferID: q.Get("transfer")}
	if args.TransferID != "" && !ops.ValidUUIDv7(args.TransferID) {
		return FilesHistoryPage{Error: "That is not a transfer id."}, nil
	}
	if args.Validate() != nil {
		args.Cursor = ""
	}
	var res ops.FileTransfersListResult
	if err := c.CallOp(r, ops.OpFileTransfersList, args, &res); err != nil {
		return FilesHistoryPage{Error: "The history could not be read: " + failureText(err)}, nil
	}
	names := workerNames(r, c, u)
	page := FilesHistoryPage{}
	for _, t := range res.Transfers {
		page.Rows = append(page.Rows, historyRowOf(t, names))
	}
	if args.TransferID != "" {
		if len(res.Transfers) == 0 {
			page.Error = "No transfer with this id is visible to you."
			return page, nil
		}
		one := page.Rows[0]
		page.One = &one
		t := res.Transfers[0]
		for _, e := range t.Entries {
			fe := FilesHistoryEntry{Path: e.Path, SHA256: e.SHA256, Skipped: skipText(e.SkippedReason)}
			if fe.Skipped == "" {
				fe.Size = formatBytes(e.Size)
			}
			page.Entries = append(page.Entries, fe)
		}
		page.Omitted = t.EntriesOmitted
		return page, nil
	}
	if res.NextCursor != "" {
		page.Next = pathFilesHistory + "?cursor=" + url.QueryEscape(res.NextCursor)
	}
	return page, nil
}

func init() {
	RegisterPage(Page{Pattern: pathFilesHistory, Title: "File transfers", Template: "files_history.html", Data: userPageData(filesHistoryData)})
	RegisterNav(NavItem{Label: "File transfers", Path: pathFilesHistory, Order: 127, Group: NavGroupWorkspace, Icon: "i-download"})
}
