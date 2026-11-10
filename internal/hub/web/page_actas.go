package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// ActAsStopURL is the operation endpoint of the banner's Exit button. The
// button posts "{}", which act_as_stop accepts (its request_id is optional).
const ActAsStopURL = ops.RESTPrefix + ops.OpActAsStop

func init() { RegisterBanner(ActAsBanner(time.Now)) }

// ActAsBanner returns the banner source of act-as (UX-DR3, R45): while the
// session of an admin acts as another user, every page shows a persistent
// warning "Acting as U (admin A) - N min left" with an Exit button. It adds
// nothing for any other session. now is the clock of the remaining time.
//
// The banner reads the session the session layer put into the request context
// ([session.Manager.Resolve] drops an act-as that has ended, so a banner never
// shows "0 min left" for a session that no longer acts). Both names are
// escaped by the layout like every other value.
func ActAsBanner(now func() time.Time) BannerFunc {
	return func(r *http.Request) []Banner {
		sess, ok := session.FromContext(r.Context())
		if !ok || sess.ActAs == nil {
			return nil
		}
		return []Banner{{
			Kind: "warning",
			Text: fmt.Sprintf("Acting as %s (admin %s) - %d min left",
				sess.ActAs.TargetUsername, sess.Username, sess.ActAs.MinutesLeft(now())),
			Action: &BannerAction{Label: "Exit", URL: ActAsStopURL},
		}}
	}
}
