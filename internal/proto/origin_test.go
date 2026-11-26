package proto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestOriginValidate(t *testing.T) {
	src := Origin{SourceWorkerID: "w1", SourceWorkerFP: "fp1", SourceWorkerOwnerID: "u1"}
	with := func(o Origin, f func(*Origin)) Origin { f(&o); return o }
	good := []Origin{
		{Kind: OriginUser, UserID: "u"},
		{Kind: OriginUser, UserID: "u", ActorID: "admin"},
		{Kind: OriginAPIKey, UserID: "u", KeyID: "k"},
		with(src, func(o *Origin) { o.Kind, o.UserID, o.KeyID, o.KeyBoundWorkerID = OriginAPIKey, "u2", "k", "w1" }),
		with(src, func(o *Origin) { o.Kind = OriginWorker }),
		with(src, func(o *Origin) { o.Kind, o.Grant = OriginWorker, true }),
	}
	for i, o := range good {
		if err := o.Validate(); err != nil {
			t.Errorf("good[%d] %+v: %v", i, o, err)
		}
	}
	bad := map[string]Origin{
		"zero":                     {},
		"unknown kind":             {Kind: "robot", UserID: "u"},
		"user without id":          {Kind: OriginUser},
		"user with key":            {Kind: OriginUser, UserID: "u", KeyID: "k"},
		"user with source":         with(src, func(o *Origin) { o.Kind, o.UserID = OriginUser, "u" }),
		"apikey without key id":    {Kind: OriginAPIKey, UserID: "u"},
		"apikey with actor":        {Kind: OriginAPIKey, UserID: "u", KeyID: "k", ActorID: "a"},
		"bound key without source": {Kind: OriginAPIKey, UserID: "u", KeyID: "k", KeyBoundWorkerID: "w1"},
		"source differs from bound": with(src, func(o *Origin) {
			o.Kind, o.UserID, o.KeyID, o.KeyBoundWorkerID = OriginAPIKey, "u", "k", "w9"
		}),
		"source without bound key": with(src, func(o *Origin) { o.Kind, o.UserID, o.KeyID = OriginAPIKey, "u", "k" }),
		"worker without source":    {Kind: OriginWorker},
		"worker with user":         with(src, func(o *Origin) { o.Kind, o.UserID = OriginWorker, "u" }),
		"partial source":           {Kind: OriginWorker, SourceWorkerID: "w1", SourceWorkerFP: "fp"},
		"missing owner":            {Kind: OriginWorker, SourceWorkerID: "w1", SourceWorkerFP: "fp"},
		"grant on a user":          {Kind: OriginUser, UserID: "u", Grant: true},
		"grant on a free key":      {Kind: OriginAPIKey, UserID: "u", KeyID: "k", Grant: true},
		"grant on a bound key": with(src, func(o *Origin) {
			o.Kind, o.UserID, o.KeyID, o.KeyBoundWorkerID, o.Grant = OriginAPIKey, "u", "k", "w1", true
		}),
		"grant without source": {Kind: OriginWorker, Grant: true},
		"control character":    {Kind: OriginUser, UserID: "u\n"},
		"too long":             {Kind: OriginUser, UserID: string(make([]byte, MaxOriginFieldLen+1))},
	}
	for name, o := range bad {
		err := o.Validate()
		if err == nil || CodeOf(err) != CodePipeOriginRefused {
			t.Errorf("%s: err = %v, want pipe_origin_refused", name, err)
		}
	}
}

const testGrantID = "01a00000-0000-7000-8000-0000000000aa"

// The attribution members (source_session, source_proof, grant_id) are
// optional and only a worker origin with a source Worker carries them.
func TestOriginValidateAttributionMembers(t *testing.T) {
	base := Origin{Kind: OriginWorker, SourceWorkerID: "w1", SourceWorkerFP: "fp1", SourceWorkerOwnerID: "u1"}
	with := func(f func(*Origin)) Origin { o := base; f(&o); return o }
	good := map[string]Origin{
		"none of them":                    base,
		"claimed session":                 with(func(o *Origin) { o.SourceSession, o.SourceProof = "planner", OriginProofClaimed }),
		"verified session":                with(func(o *Origin) { o.SourceSession, o.SourceProof = "planner", OriginProofVerified }),
		"session with a space":            with(func(o *Origin) { o.SourceSession, o.SourceProof = "my planner", OriginProofClaimed }),
		"session with a non-ASCII letter": with(func(o *Origin) { o.SourceSession, o.SourceProof = "pl\u00e4ne", OriginProofClaimed }),
		"longest session name": with(func(o *Origin) {
			o.SourceSession, o.SourceProof = strings.Repeat("s", MaxMasterSessionName), OriginProofClaimed
		}),
		"grant without grant_id": with(func(o *Origin) { o.Grant = true }),
		"grant with grant_id":    with(func(o *Origin) { o.Grant, o.GrantID = true, testGrantID }),
		"session, proof and grant": with(func(o *Origin) {
			o.SourceSession, o.SourceProof, o.Grant, o.GrantID = "planner", OriginProofClaimed, true, testGrantID
		}),
		"session without a grant": with(func(o *Origin) { o.SourceSession, o.SourceProof = "planner", OriginProofClaimed }),
	}
	for name, o := range good {
		if err := o.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]Origin{
		"session with a colon":     with(func(o *Origin) { o.SourceSession, o.SourceProof = "a:b", OriginProofClaimed }),
		"session with a dot":       with(func(o *Origin) { o.SourceSession, o.SourceProof = "a.b", OriginProofClaimed }),
		"session with a newline":   with(func(o *Origin) { o.SourceSession, o.SourceProof = "a\nb", OriginProofClaimed }),
		"session with outer space": with(func(o *Origin) { o.SourceSession, o.SourceProof = " a", OriginProofClaimed }),
		"session too long": with(func(o *Origin) {
			o.SourceSession, o.SourceProof = strings.Repeat("s", MaxMasterSessionName+1), OriginProofClaimed
		}),
		"session of invalid UTF-8": with(func(o *Origin) { o.SourceSession, o.SourceProof = "a\xffb", OriginProofClaimed }),
		"session without proof":    with(func(o *Origin) { o.SourceSession = "planner" }),
		"proof without session":    with(func(o *Origin) { o.SourceProof = OriginProofClaimed }),
		"unknown proof":            with(func(o *Origin) { o.SourceSession, o.SourceProof = "planner", "bound" }),
		"proof in another case":    with(func(o *Origin) { o.SourceSession, o.SourceProof = "planner", "Claimed" }),
		"grant_id without grant":   with(func(o *Origin) { o.GrantID = testGrantID }),
		"grant_id not a UUID":      with(func(o *Origin) { o.Grant, o.GrantID = true, "grant-1" }),
		"grant_id of version 4":    with(func(o *Origin) { o.Grant, o.GrantID = true, "01a00000-0000-4000-8000-0000000000aa" }),
		"grant_id in upper case":   with(func(o *Origin) { o.Grant, o.GrantID = true, "01A00000-0000-7000-8000-0000000000AA" }),
		"user with a session":      {Kind: OriginUser, UserID: "u", SourceSession: "planner", SourceProof: OriginProofClaimed},
		"user with a grant id":     {Kind: OriginUser, UserID: "u", GrantID: testGrantID},
		"free key with a session":  {Kind: OriginAPIKey, UserID: "u", KeyID: "k", SourceSession: "planner", SourceProof: OriginProofClaimed},
		"bound key with a session": {Kind: OriginAPIKey, UserID: "u", KeyID: "k", KeyBoundWorkerID: "w1", SourceWorkerID: "w1",
			SourceWorkerFP: "fp1", SourceWorkerOwnerID: "u1", SourceSession: "planner", SourceProof: OriginProofClaimed},
		"worker without source": {Kind: OriginWorker, SourceSession: "planner", SourceProof: OriginProofClaimed},
	}
	for name, o := range bad {
		err := o.Validate()
		if err == nil || CodeOf(err) != CodePipeOriginRefused {
			t.Errorf("%s: err = %v, want pipe_origin_refused", name, err)
			continue
		}
		// The error never echoes a value of the origin.
		for _, v := range []string{o.SourceSession, o.GrantID} {
			if len(v) > 3 && strings.Contains(err.Error(), v) {
				t.Errorf("%s: the error echoes a value: %v", name, err)
			}
		}
	}
}

// The attribution members travel as source_session, source_proof and grant_id,
// are left out when empty, and a decoder that does not know them (an older
// Worker decodes the origin without DisallowUnknownFields) still reads the
// rest and finds it valid.
func TestOriginAttributionMembersOnTheWire(t *testing.T) {
	full := Origin{Kind: OriginWorker, SourceWorkerID: "w1", SourceWorkerFP: "fp1", SourceWorkerOwnerID: "u1",
		Grant: true, SourceSession: "my planner", SourceProof: OriginProofClaimed, GrantID: testGrantID}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["source_session"] != "my planner" || m["source_proof"] != "claimed" || m["grant_id"] != testGrantID {
		t.Fatalf("wire form = %s", b)
	}
	var back Origin
	if err := json.Unmarshal(b, &back); err != nil || back != full {
		t.Fatalf("round trip = %+v (%v), want %+v", back, err, full)
	}

	plain, err := json.Marshal(Origin{Kind: OriginUser, UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"source_session", "source_proof", "grant_id"} {
		if strings.Contains(string(plain), member) {
			t.Errorf("an origin without %s still writes it: %s", member, plain)
		}
	}

	// The origin as a Worker of the release before these members sees it.
	type oldOrigin struct {
		Kind                OriginKind `json:"kind"`
		UserID              string     `json:"user_id,omitempty"`
		ActorID             string     `json:"actor_id,omitempty"`
		KeyID               string     `json:"key_id,omitempty"`
		KeyBoundWorkerID    string     `json:"key_bound_worker_id,omitempty"`
		SourceWorkerID      string     `json:"source_worker_id,omitempty"`
		SourceWorkerFP      string     `json:"source_worker_fp,omitempty"`
		SourceWorkerOwnerID string     `json:"source_worker_owner_id,omitempty"`
		Grant               bool       `json:"grant,omitempty"`
	}
	var old oldOrigin
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&old); err != nil || dec.More() {
		t.Fatalf("an old decoder refuses the new members: %v", err)
	}
	seen := Origin{Kind: old.Kind, SourceWorkerID: old.SourceWorkerID, SourceWorkerFP: old.SourceWorkerFP,
		SourceWorkerOwnerID: old.SourceWorkerOwnerID, Grant: old.Grant}
	if err := seen.Validate(); err != nil {
		t.Fatalf("what an old Worker reads is not valid: %v", err)
	}
	if !seen.Grant || seen.SourceWorkerID != "w1" {
		t.Fatalf("an old Worker reads %+v", seen)
	}
}
