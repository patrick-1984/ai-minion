//go:build !android

package hubdial

import (
	"net"
	"reflect"
	"testing"
)

// Outside Android the override never applies, whatever the environment says.
func TestDialIsTheIdentityOutsideAndroid(t *testing.T) {
	t.Setenv(EnvName, "minion.example=192.0.2.10")
	if Dial(nil) != nil {
		t.Fatal("Dial(nil) must stay nil, so http.Transport keeps its own default")
	}
	next := (&net.Dialer{}).DialContext
	got := Dial(next)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(DialFunc(next)).Pointer() {
		t.Fatal("Dial(next) must return next unchanged")
	}
}
