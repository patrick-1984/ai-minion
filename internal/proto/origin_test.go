package proto

import "testing"

func TestOriginValidate(t *testing.T) {
	src := Origin{SourceWorkerID: "w1", SourceWorkerFP: "fp1", SourceWorkerOwnerID: "u1"}
	with := func(o Origin, f func(*Origin)) Origin { f(&o); return o }
	good := []Origin{
		{Kind: OriginUser, UserID: "u"},
		{Kind: OriginUser, UserID: "u", ActorID: "admin"},
		{Kind: OriginAPIKey, UserID: "u", KeyID: "k"},
		with(src, func(o *Origin) { o.Kind, o.UserID, o.KeyID, o.KeyBoundWorkerID = OriginAPIKey, "u2", "k", "w1" }),
		with(src, func(o *Origin) { o.Kind = OriginWorker }),
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
		"control character":        {Kind: OriginUser, UserID: "u\n"},
		"too long":                 {Kind: OriginUser, UserID: string(make([]byte, MaxOriginFieldLen+1))},
	}
	for name, o := range bad {
		err := o.Validate()
		if err == nil || CodeOf(err) != CodePipeOriginRefused {
			t.Errorf("%s: err = %v, want pipe_origin_refused", name, err)
		}
	}
}
