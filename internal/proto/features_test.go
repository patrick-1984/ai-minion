package proto

import (
	"slices"
	"testing"
)

// TestSessionInstanceFeatureIsAnnounceable: the name passes the filter of the
// signed hello, so a Worker that lists it really announces it.
func TestSessionInstanceFeatureIsAnnounceable(t *testing.T) {
	if FeatureSessionInstanceV1 != "session.instance.v1" {
		t.Fatalf("FeatureSessionInstanceV1 = %q: the name is wire format", FeatureSessionInstanceV1)
	}
	got := NormaliseFeatures([]string{FeatureTextVerbatim, FeatureSessionInstanceV1, FeatureAnswerReviewed, FeatureSessionInstanceV1})
	want := []string{FeatureAnswerReviewed, FeatureSessionInstanceV1, FeatureTextVerbatim}
	if !slices.Equal(got, want) {
		t.Fatalf("NormaliseFeatures = %v, want %v", got, want)
	}
	if !HasFeature(got, FeatureSessionInstanceV1) || HasFeature([]string{FeatureAnswerReviewed}, FeatureSessionInstanceV1) {
		t.Error("HasFeature does not follow the list")
	}
}

// TestSessionInstanceIDListFeatureIsAnnounceable: the listing-only feature is
// a name of its own, distinct from the reserved full instance contract, and
// passes the filter of the signed hello.
func TestSessionInstanceIDListFeatureIsAnnounceable(t *testing.T) {
	if FeatureSessionInstanceIDListV1 != "session.instance_id.list.v1" {
		t.Fatalf("FeatureSessionInstanceIDListV1 = %q: the name is wire format", FeatureSessionInstanceIDListV1)
	}
	got := NormaliseFeatures([]string{FeatureSessionInstanceIDListV1})
	if !slices.Equal(got, []string{FeatureSessionInstanceIDListV1}) {
		t.Fatalf("NormaliseFeatures = %v", got)
	}
	if HasFeature(got, FeatureSessionInstanceV1) {
		t.Fatal("the listing feature must not read as the full instance contract")
	}
}
