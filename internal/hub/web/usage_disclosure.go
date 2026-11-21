package web

import (
	"net/http"
	"strings"
)

func init() {
	RegisterBanner(func(r *http.Request) []Banner {
		if r.URL.Path == "/workers/add" || strings.HasPrefix(r.URL.Path, "/s/") || strings.HasPrefix(r.URL.Path, "/workers/") {
			return []Banner{{Kind: "info", Text: "Workers enrolled in usage monitoring report session names, folders, token counts and collection diagnostics to their owner and Hub administrators. This includes unshared sessions. Console access uses your session sharing permissions."}}
		}
		return nil
	})
}
