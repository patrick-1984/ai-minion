package server

import (
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
)

func TestOnlyABrowserSessionActingAsItselfReplacesTheHub(t *testing.T) {
	session := &api.Call{}
	session.Principal = authz.Principal{Kind: authz.KindBrowserSession, UserID: "admin"}
	if !mayReplaceHub(session) {
		t.Error("an administrator's browser session was refused")
	}
	actAs := &api.Call{}
	actAs.Principal = authz.NewActAsPrincipal("admin", "u2", time.Now().Add(time.Hour))
	actAs.Identity.ActingAs = "u2"
	key := &api.Call{}
	key.Principal = authz.Principal{Kind: authz.KindAPIKey, UserID: "admin"}
	worker := &api.Call{}
	worker.Principal = authz.Principal{Kind: authz.KindWorker}
	for name, c := range map[string]*api.Call{"act-as": actAs, "api key": key, "worker": worker} {
		if mayReplaceHub(c) {
			t.Errorf("%s may replace the Hub", name)
		}
	}
}
