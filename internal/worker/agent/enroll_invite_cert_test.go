package agent

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// An invitation names the ROOT: the fingerprint of the operational key must not
// match it, and the secret never leaves the Worker (design section 12.1).
func TestInvitationFingerprintIsComparedWithTheRootNotTheOperationalKey(t *testing.T) {
	hub, _ := newInviteHub(t)
	opInvite, err := identity.NewInviteKey(hub.hub.Op.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	logs := &logBuf{}
	res := inviteResolver(config.MapSource{config.KeyServer: hub.url(), config.KeyInviteKey: opInvite.String()}, nil, nil)
	s := inviteSettings(t, res, &out, logs)
	err = runHook(t, s, "", &stderr)
	if !errors.Is(err, transport.ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want a fingerprint mismatch", err)
	}
	if got := hub.secretsReceived(); len(got) != 0 {
		t.Fatalf("the secret was sent: %v", got)
	}
	if _, ok, _ := openWorkerState(t, s.StateDir).Load(context.Background()); ok {
		t.Fatal("an enrollment was stored")
	}
}

func TestInvitationEnrollmentCommitsRootPinChainZeroAndTheCertificateRatchetOnlyAfterWelcome(t *testing.T) {
	hub, invite := newInviteHub(t)
	hub.hub.Reissue(t, 42)
	var out, stderr bytes.Buffer
	res := inviteResolver(config.MapSource{config.KeyServer: hub.url(), config.KeyInviteKey: invite.String()}, nil, nil)
	s := inviteSettings(t, res, &out, &logBuf{})
	if err := runHook(t, s, "", &stderr); err != nil {
		t.Fatal(err)
	}
	enr, ok, err := openWorkerState(t, s.StateDir).Load(context.Background())
	if err != nil || !ok {
		t.Fatalf("enrollment: %v %v", ok, err)
	}
	if !bytes.Equal(enr.Pin.Pub, hub.key.Public()) || enr.Pin.Seq != 0 {
		t.Fatalf("pin = %+v, want the root at chain position 0", enr.Pin)
	}
	if enr.Ratchet.MaxSerial != 42 || !bytes.Equal(enr.Ratchet.CertSHA256, hashOf(hub.hub.Cert)) || !bytes.Equal(enr.Ratchet.Root, hub.key.Public()) {
		t.Fatalf("ratchet = %+v", enr.Ratchet)
	}
}

func TestInvitationWithoutAWelcomeLeavesNoPinSerialOrCertificateHash(t *testing.T) {
	hub, invite := newInviteHub(t)
	hub.loseAnswer = true
	var out, stderr bytes.Buffer
	res := inviteResolver(config.MapSource{config.KeyServer: hub.url(), config.KeyInviteKey: invite.String()}, nil, nil)
	s := inviteSettings(t, res, &out, &logBuf{})
	d := defaultInviteHookDeps()
	d.stdin, d.stderr, d.attempts = bytes.NewReader(nil), &stderr, 1
	if err := d.run(context.Background(), s); err == nil {
		t.Fatal("no error without a welcome")
	}
	if _, ok, _ := openWorkerState(t, s.StateDir).Load(context.Background()); ok {
		t.Fatal("a pin, serial or hash was stored before the welcome")
	}
}
