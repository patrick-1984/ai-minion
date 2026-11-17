package agent

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// A dial or update error never carries a URL's credential, path or query
// into the log, the remote ring, the connection status or the update status.
func TestErrStringCutsURLsToSchemeAndHost(t *testing.T) {
	ue := &url.Error{Op: "Get", URL: "https://us@er:p@ss@hub.example/s3cr3t/api?k=v", Err: errors.New("EOF")}
	for _, err := range []error{ue, fmt.Errorf("dial: %w", ue), errors.New(`download "https://us@er:p@ss@mirror/tok3n/x": 404`)} {
		got := errString(err)
		for _, leak := range []string{"us@", "p@ss", "s3cr3t", "k=v", "tok3n"} {
			if strings.Contains(got, leak) {
				t.Errorf("errString(%v) = %q leaks %q", err, got, leak)
			}
		}
	}
	if got := causeText(ue); strings.Contains(got, "p@ss") || !strings.Contains(got, "https://hub.example") {
		t.Errorf("causeText = %q", got)
	}
}
