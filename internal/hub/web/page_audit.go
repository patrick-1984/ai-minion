package web

// Administration pages, part 4: the audit log (Story 6.8, AD-20). Events
// are listed newest first, with filters (actor, action, time range) and
// cursor paging. The page only reads: audit_list is the one operation, and no
// event can be changed here.

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// auditLimits are the page sizes offered by the filter form.
var auditLimits = []int{25, 50, 100, 200}

// AuditFilter is the filter form, as typed.
type AuditFilter struct {
	Actor  string
	Action string
	// From and To are dates ("2026-11-09") or RFC 3339 times, in UTC. From is
	// inclusive; a date in To includes that whole day.
	From  string
	To    string
	Limit int
}

// AuditPage is what audit.html sees as .Data.
type AuditPage struct {
	Filter AuditFilter
	Limits []int
	Events []ops.AuditEvent
	// Error explains a filter that could not be used; Events is empty then.
	Error string
	// NextURL is the link to the next (older) page; empty on the last page.
	NextURL string
	// FirstURL is the link back to the newest events, set on a later page.
	FirstURL string
}

// parseAuditTime reads a filter time: a date, or RFC 3339. endOfDay makes a
// date mean the end of that day (the first instant of the next one), so that a
// "to" date includes the day.
func parseAuditTime(s string, endOfDay bool) (*ops.Timestamp, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var t time.Time
	if d, err := time.Parse(time.DateOnly, s); err == nil {
		t = d
		if endOfDay {
			t = t.AddDate(0, 0, 1)
		}
	} else if ts, err := time.Parse(time.RFC3339, s); err == nil {
		t = ts.UTC()
	} else {
		return nil, errBadTime
	}
	ts := ops.Timestamp(t.UTC().Truncate(time.Millisecond))
	return &ts, nil
}

type auditError string

func (e auditError) Error() string { return string(e) }

const errBadTime = auditError("Enter dates as 2026-11-09 or as an RFC 3339 time such as 2026-11-09T14:30:00Z.")

// auditArgs turns the query of the page into the arguments of audit_list.
func auditArgs(q url.Values) (ops.AuditListArgs, AuditFilter, error) {
	f := AuditFilter{
		Actor: strings.TrimSpace(q.Get("actor")), Action: strings.TrimSpace(q.Get("action")),
		From: strings.TrimSpace(q.Get("from")), To: strings.TrimSpace(q.Get("to")),
		Limit: ops.DefaultAuditListLimit,
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > ops.MaxAuditListLimit {
			return ops.AuditListArgs{}, f, auditError("The page size must be a number from 1 to " + strconv.Itoa(ops.MaxAuditListLimit) + ".")
		}
		f.Limit = n
	}
	args := ops.AuditListArgs{Actor: f.Actor, Action: f.Action, Cursor: strings.TrimSpace(q.Get("cursor")), Limit: f.Limit}
	var err error
	if args.From, err = parseAuditTime(f.From, false); err != nil {
		return args, f, err
	}
	if args.To, err = parseAuditTime(f.To, true); err != nil {
		return args, f, err
	}
	return args, f, nil
}

// auditLink builds a link to the audit page with the filter and a cursor.
func auditLink(f AuditFilter, cursor string) string {
	q := url.Values{}
	for k, v := range map[string]string{"actor": f.Actor, "action": f.Action, "from": f.From, "to": f.To, "cursor": cursor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.Limit != ops.DefaultAuditListLimit {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if len(q) == 0 {
		return pathAdminAudit
	}
	return pathAdminAudit + "?" + q.Encode()
}

func auditData(r *http.Request, c OpCaller) (any, error) {
	args, filter, err := auditArgs(r.URL.Query())
	page := AuditPage{Filter: filter, Limits: auditLimits}
	if err != nil {
		page.Error = err.Error()
		return page, nil
	}
	var res ops.AuditListResult
	if err := c.CallOp(r, ops.OpAuditList, args, &res); err != nil {
		var oe *OpError
		if errors.As(err, &oe) && oe.Status == http.StatusBadRequest {
			// The operation refused the filter (for example "to before from").
			page.Error = opMessage(oe)
			return page, nil
		}
		return nil, err
	}
	page.Events = res.Events
	if res.NextCursor != "" {
		page.NextURL = auditLink(filter, res.NextCursor)
	}
	if args.Cursor != "" {
		page.FirstURL = auditLink(filter, "")
	}
	return page, nil
}

func init() {
	RegisterPage(Page{Pattern: pathAdminAudit, Title: "Audit", Template: "audit.html", Data: adminPageData(auditData)})
	RegisterNav(NavItem{Label: "Audit", Path: pathAdminAudit, Order: adminNavOrder + 30, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-audit"})
}
