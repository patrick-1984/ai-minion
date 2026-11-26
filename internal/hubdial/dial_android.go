//go:build android

package hubdial

import (
	"net/http"
	"os"
	"sync"
)

var active = sync.OnceValues(func() (*Override, error) { return Parse(os.Getenv(EnvName)) })

// Dial wraps next with the override from [EnvName]. Without one it returns
// next unchanged; with an invalid one every dial fails with the reason.
func Dial(next DialFunc) DialFunc {
	o, err := active()
	return dialWith(o, err, next)
}

// The default transport (cloned by the OTLP exporters, used by any plain
// http.Get) follows the override as well.
func init() {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		if o, err := active(); err == nil && o != nil {
			t.DialContext = o.Wrap(t.DialContext)
		}
	}
}
