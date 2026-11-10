package web

// The "Add worker" page (Story 6.7, R54, AD-8). The user types the pairing code
// that a Worker shows (MN-XXXX-XXXX, any letter case, hyphens optional) and an
// optional name. The post is one pairing_claim call as the signed-in user, so
// the Worker joins that user's space; the Hub counts the attempts. The code is
// a secret until used: it is never logged, put into a flash message or echoed
// back.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// AddWorkerPage is what addworker.html sees as .Data.
type AddWorkerPage struct {
	MaxName int
}

func addWorkerData(_ *http.Request, _ OpCaller, _ pageUser) (any, error) {
	return AddWorkerPage{MaxName: ops.MaxWorkerNameLen}, nil
}

// pairingErrorText turns a failed pairing_claim into a plain-English sentence.
// Codes it does not know get the message of the Hub.
func pairingErrorText(err error) string {
	var oe *OpError
	if !errors.As(err, &oe) {
		return "The Hub could not complete the request. Try again."
	}
	switch proto.Code(oe.Code) {
	case proto.CodePairingCodeInvalid:
		return "That code is not right or has expired. Check the code on the Worker's screen and try again."
	case proto.CodePairingAttemptsExceeded:
		return "Too many wrong attempts. That code no longer works. Start pairing again on the Worker to get a new code."
	case proto.CodeWorkerOffline:
		return "The Worker that shows this code is not connected to the Hub right now. Check that it is running and can reach this Hub, then try again."
	case proto.CodeRateLimited:
		return "Too many attempts in a short time. Wait a minute and try again."
	case proto.CodeAlreadyPaired:
		return "This Worker is already paired."
	case proto.CodeForbidden, proto.CodeUnauthenticated:
		return "You are not allowed to add a Worker. Sign in again."
	}
	return failureText(err)
}

// addedWorker is what the worker_added fragment renders.
type addedWorker struct {
	Name     string
	Warnings []string
}

var addWorkerFragments = fragments("addworker.html")

// workerIDs lists the Worker ids of the user, so that the Worker a claim
// created can be found afterwards: pairing_claim answers with the outcome only.
// A failing list yields nil, and the page then names the Worker by the name
// the user typed or by a neutral word.
func workerIDs(r *http.Request, c OpCaller) map[string]bool {
	var res ops.WorkersListResult
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{}, &res); err != nil {
		return nil
	}
	ids := make(map[string]bool, len(res.Workers))
	for _, w := range res.Workers {
		ids[w.ID] = true
	}
	return ids
}

// newWorkerName finds the Worker that the claim created: the one the user did
// not have before.
func newWorkerName(r *http.Request, c OpCaller, before map[string]bool) string {
	if before == nil {
		return ""
	}
	var res ops.WorkersListResult
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{}, &res); err != nil {
		return ""
	}
	for _, w := range res.Workers {
		if !before[w.ID] {
			return w.Name
		}
	}
	return ""
}

func handleAddWorker(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	code, err := identity.ParsePairingCode(strings.TrimSpace(r.PostFormValue("code")))
	if err != nil {
		writeResultError(w, "That is not a pairing code. It looks like MN-ABCD-EFGH: eight letters and digits, shown on the Worker.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name != "" && !ops.ValidWorkerName(name) {
		writeResultError(w, "The name can have at most "+strconv.Itoa(ops.MaxWorkerNameLen)+" characters, without spaces or colons.")
		return
	}
	before := workerIDs(r, c)
	args := ops.PairingClaimArgs{WriteArgs: newWriteArgs(), Code: code.String(), Name: name}
	var res ops.WriteResult
	if err := c.CallOp(r, ops.OpPairingClaim, args, &res); err != nil {
		writeResultError(w, pairingErrorText(err))
		return
	}
	if res.Outcome != "" && res.Outcome != ops.OutcomeDelivered {
		writeResultError(w, "The Worker could not be added"+reasonSuffix(res.Reason)+". Try again.")
		return
	}
	out := addedWorker{Name: newWorkerName(r, c, before)}
	if out.Name == "" {
		out.Name = name
	}
	if out.Name == "" {
		out.Name = "The Worker"
	}
	for _, wn := range res.Warnings {
		out.Warnings = append(out.Warnings, wn.Message)
	}
	writeFragmentTemplate(w, addWorkerFragments(), "worker_added", out)
}

func init() {
	RegisterPage(Page{Pattern: pathAddWorker, Title: "Add worker", Template: "addworker.html", Data: userPageData(addWorkerData)})
	RegisterNav(NavItem{Label: "Add worker", Path: pathAddWorker, Order: onboardingNavOrder - 10, Group: NavGroupAccess, Icon: "i-plus"})
	RegisterRoute(Route{Pattern: "POST " + pathAddWorker, Handler: userRoute(handleAddWorker)})
}
