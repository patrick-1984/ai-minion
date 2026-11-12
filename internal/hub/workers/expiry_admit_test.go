package workers

import (
	"testing"
	"time"
)

// A handshake that starts before the certificate's not_after and reaches the
// registry after it is refused at registration, and no session survives.
func TestAHandshakeThatStartsBeforeNotAfterIsRefusedWhenItRegistersAfterIt(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "o", "pi")
	notAfter := time.UnixMilli(h.hub.Cert.NotAfterMs)
	h.svc.handler.beforeRegister = func() {
		// The clock passes not_after while the handshake is stalled.
		h.id.SetNow(func() time.Time { return notAfter.Add(time.Second) })
	}
	_, s := connectWelcomed(t, h, key)
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled handshake kept a session after the certificate expired")
	}
	if h.svc.Registry().IsOnline(w.ID) {
		t.Error("the Worker registered after not_after")
	}
}
