package agent

import (
	"context"
	"testing"
	"time"
)

type pauseLive struct{ paused *bool }

func (l pauseLive) LiveStatus(context.Context) (LiveStatus, error) {
	return LiveStatus{Connector: Status{State: ConnIdle}, Paused: l.paused}, nil
}

func TestStatusPauseFields(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	yes, no := true, false
	timed := PauseState{Paused: true, Until: now.Add(time.Hour), Generation: 3}
	for _, tc := range []struct {
		name    string
		saved   PauseState
		live    LiveSource
		paused  bool
		until   string
		inForce *bool
	}{
		{name: "timed, no Worker runs", saved: timed, paused: true, until: "2026-11-18T13:00:00Z", inForce: &yes},
		{name: "timed, Worker enforces it", saved: timed, live: pauseLive{&yes}, paused: true, until: "2026-11-18T13:00:00Z", inForce: &yes},
		{name: "timed, Worker not yet", saved: timed, live: pauseLive{&no}, paused: true, until: "2026-11-18T13:00:00Z", inForce: &no},
		{name: "saved, Worker without pause support", saved: PauseState{Paused: true}, live: pauseLive{}, paused: true},
		{name: "expired", saved: PauseState{Paused: true, Until: now.Add(-time.Second)}, paused: false, inForce: &yes},
		{name: "resumed, Worker still paused", saved: PauseState{Generation: 4}, live: pauseLive{&yes}, paused: false, inForce: &no},
		{name: "never paused, old Worker", live: pauseLive{}, paused: false, inForce: &yes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := tc.saved
			r := BuildStatus(context.Background(), StatusInputs{
				HubURL: "https://hub.example", Live: tc.live, Now: func() time.Time { return now },
				Pause: func() (PauseState, error) { return saved, nil },
			})
			if r.Paused != tc.paused || r.PausedUntil != tc.until {
				t.Fatalf("paused %t until %q; want %t %q", r.Paused, r.PausedUntil, tc.paused, tc.until)
			}
			switch {
			case tc.inForce == nil && r.PauseInForce != nil:
				t.Fatalf("pause_in_force = %t, want null", *r.PauseInForce)
			case tc.inForce != nil && (r.PauseInForce == nil || *r.PauseInForce != *tc.inForce):
				t.Fatalf("pause_in_force = %v, want %t", r.PauseInForce, *tc.inForce)
			}
		})
	}
}
