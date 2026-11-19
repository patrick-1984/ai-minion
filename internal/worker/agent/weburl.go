package agent

import (
	"net/url"
	"strings"
)

// HubWebURL returns the browser URL of the Hub whose configured URL is
// hubURL (tray W10b): ws becomes http and wss becomes https, the base path is
// kept, and the user info, the query and the fragment are dropped, so that no
// credential configured into the URL reaches a browser, a log or the status.
// It is the reverse of transport.ConnectURL without the connect path. An
// empty, unparsable or non-web URL gives "".
func HubWebURL(hubURL string) string {
	u, err := url.Parse(strings.TrimSpace(hubURL))
	if err != nil || u.Host == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		u.Scheme = "https"
	case "http", "ws":
		u.Scheme = "http"
	default:
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	u.Opaque = ""
	return u.String()
}
