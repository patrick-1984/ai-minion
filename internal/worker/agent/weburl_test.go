package agent

import "testing"

// The browser URL of the Hub: ws/wss become http/https, the base path stays,
// user info, query and fragment are dropped (tray W10b).
func TestWebURL(t *testing.T) {
	for in, want := range map[string]string{
		"wss://u:p@h/base/":                    "https://h/base/",
		"ws://h:8080":                          "http://h:8080",
		"https://hub.example:8443/minion":      "https://hub.example:8443/minion",
		"HTTP://user@hub.example/?token=x#top": "http://hub.example/",
		" wss://h/a%2Fb ":                      "https://h/a%2Fb",
		"":                                     "",
		"ftp://h/":                             "",
		"https://":                             "",
		"not a url":                            "",
	} {
		if got := HubWebURL(in); got != want {
			t.Errorf("HubWebURL(%q) = %q, want %q", in, got, want)
		}
	}
}
