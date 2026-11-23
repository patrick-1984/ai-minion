package store

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestUsageMigrationGrandfathersExistingEnrollmentAndFencesChanges(t *testing.T) {
	legacy := fstest.MapFS{}
	entries, err := fs.ReadDir(workerMigrations, migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range entries {
		if file.IsDir() || file.Name() >= "0012_" {
			continue
		}
		path := migrationsDir + "/" + file.Name()
		body, err := fs.ReadFile(workerMigrations, path)
		if err != nil {
			t.Fatal(err)
		}
		legacy[path] = &fstest.MapFile{Data: body}
	}

	for _, state := range []struct {
		name, workerID, owner string
	}{
		{"prior enrollment", "grandfathered-worker", "original-owner"},
		{"previous pin without a welcome", "", ""},
	} {
		t.Run(state.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			prior := Enrollment{HubPub: pubKey(1), WorkerID: state.workerID, Owner: state.owner}
			old := openStore(t, dir, WithMigrations(legacy, migrationsDir))
			if err := old.SaveEnrollment(ctx, prior); err != nil {
				t.Fatal(err)
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			s := openStore(t, dir)
			check := func(worker string, allowed, consented bool) {
				t.Helper()
				gotAllowed, err := s.UsageConnectionAllowed(ctx, worker)
				if err != nil || gotAllowed != allowed {
					t.Fatalf("link admission for %s = %t, %v, want %t", worker, gotAllowed, err, allowed)
				}
				gotConsent, err := s.UsageRegistration(ctx, worker)
				if err != nil || gotConsent != consented {
					t.Fatalf("telemetry consent for %s = %t, %v, want %t", worker, gotConsent, err, consented)
				}
			}
			welcome := WelcomeInfo{WorkerID: prior.WorkerID, Owner: prior.Owner, Name: "workstation", Kind: "private"}
			if prior.WorkerID == "" {
				welcome.WorkerID, welcome.Owner = "new-worker", "new-owner"
			}
			if err := s.UpdateWelcome(ctx, welcome); err != nil {
				t.Fatal(err)
			}
			check(welcome.WorkerID, prior.WorkerID != "", false)
			accept := func(owner string) {
				t.Helper()
				en, err := s.LoadEnrollment(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.AcceptUsageRegistration(ctx, en.HubPub, en.WorkerID, owner); err != nil {
					t.Fatal(err)
				}
				check(en.WorkerID, true, true)
			}
			accept(welcome.Owner)

			former := welcome.Owner
			welcome.Owner = "different-owner"
			if err := s.UpdateWelcome(ctx, welcome); err != nil {
				t.Fatal(err)
			}
			check(welcome.WorkerID, false, false)
			if err := s.AcceptUsageRegistration(ctx, prior.HubPub, welcome.WorkerID, former); !errors.Is(err, ErrNotEnrolled) {
				t.Fatalf("old owner accepted current policy: %v", err)
			}
			accept(welcome.Owner)

			if err := s.UpdatePin(ctx, pubKey(2), 1); err != nil {
				t.Fatal(err)
			}
			check(welcome.WorkerID, false, false)
			if err := s.AcceptUsageRegistration(ctx, prior.HubPub, welcome.WorkerID, welcome.Owner); !errors.Is(err, ErrNotEnrolled) {
				t.Fatalf("old Hub identity accepted current policy: %v", err)
			}
			accept(welcome.Owner)
		})
	}
}
