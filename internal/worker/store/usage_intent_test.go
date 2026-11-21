package store

import (
	"context"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

func TestUsageIntentVerifiedWelcomeConsumesOnce(t *testing.T) {
	ctx := context.Background()
	for _, spec := range []struct {
		name   string
		policy int
		target string
		want   bool
	}{
		{"modern", 1, "https://hub.example", true}, {"legacy", 0, "https://hub.example", false}, {"newer-policy", 2, "https://hub.example", false}, {"different-hub", 1, "https://other.example", false},
	} {
		t.Run(spec.name, func(t *testing.T) {
			s := openStore(t, t.TempDir())
			pub := pubKey(7)
			fp, err := identity.FingerprintOf(pub)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.PrepareUsageIntent(ctx, "https://hub.example", fp.String()); err != nil {
				t.Fatal(err)
			}
			en := Enrollment{HubPub: pub, WorkerID: "work", Owner: "owner"}
			if err := s.SaveEnrollment(ctx, en); err != nil {
				t.Fatal(err)
			}
			if err := s.ConsumeUsageIntent(ctx, spec.target, "work", "owner", pub, spec.policy); err != nil {
				t.Fatal(err)
			}
			ok, err := s.UsageRegistration(ctx, "work")
			if err != nil || ok != spec.want {
				t.Fatalf("approved=%t err=%v", ok, err)
			}
			if _, err := s.db.Exec(`UPDATE hub_enrollment SET usage_policy=0 WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			if err := s.ConsumeUsageIntent(ctx, "https://hub.example", "work", "owner", pub, 1); err != nil {
				t.Fatal(err)
			}
			if ok, _ := s.UsageRegistration(ctx, "work"); ok {
				t.Fatal("replayed prior consent")
			}
		})
	}
}

func TestUsageIntentTVPairingExpiryAndOwnerBinding(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	server := "https://hub.example"
	en := Enrollment{HubPub: pubKey(8), WorkerID: "work", Owner: "alice"}
	if err := s.PrepareUsageIntent(ctx, server, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEnrollment(ctx, en); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeUsageIntent(ctx, server, en.WorkerID, en.Owner, en.HubPub, 1); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.UsageRegistration(ctx, en.WorkerID); !ok {
		t.Fatal("trusted TV welcome did not consume consent")
	}
	if err := s.PrepareUsageIntent(ctx, server, ""); err != nil {
		t.Fatal(err)
	}
	en.Owner = "bob"
	if err := s.SaveEnrollment(ctx, en); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeUsageIntent(ctx, server, en.WorkerID, en.Owner, en.HubPub, 1); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.UsageRegistration(ctx, en.WorkerID); ok {
		t.Fatal("previous owner granted new enrollment")
	}
	if err := s.PrepareUsageIntent(ctx, server, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE usage_registration_intent SET expires_at=1`); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeUsageIntent(ctx, server, en.WorkerID, en.Owner, en.HubPub, 1); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.UsageRegistration(ctx, en.WorkerID); ok {
		t.Fatal("expired intent enabled monitoring")
	}
}
