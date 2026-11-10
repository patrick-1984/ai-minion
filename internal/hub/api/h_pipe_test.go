package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakePipe is a PipeService that records what the handlers hand it.
type fakePipe struct {
	mu       sync.Mutex
	sends    []ops.PipeSendArgs
	reads    []ops.PipeReadArgs
	waits    []ops.PipeWaitArgs
	who      []authz.Principal
	sendRes  ops.WriteResult
	readRes  workers.PipeReadView
	waitTurn []ops.PipeTurn
	err      error
}

func (f *fakePipe) Send(_ context.Context, p authz.Principal, a ops.PipeSendArgs) (ops.WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends, f.who = append(f.sends, a), append(f.who, p)
	return f.sendRes, f.err
}

func (f *fakePipe) Read(_ context.Context, p authz.Principal, a ops.PipeReadArgs) (workers.PipeReadView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads, f.who = append(f.reads, a), append(f.who, p)
	return f.readRes, f.err
}

func (f *fakePipe) Wait(_ context.Context, p authz.Principal, a ops.PipeWaitArgs, emit func(ops.PipeTurn) error) error {
	f.mu.Lock()
	f.waits, f.who = append(f.waits, a), append(f.who, p)
	turns, err := f.waitTurn, f.err
	f.mu.Unlock()
	for _, t := range turns {
		if e := emit(t); e != nil {
			return e
		}
	}
	return err
}

func TestRegisterWorkerOpsWithPipeServesPipeOpsFromTheService(t *testing.T) {
	reg := NewRegistry(ops.Default)
	rt := &woRouter{}
	mustRegister(t, RegisterWorkerOpsWithPipe(reg, rt, &fakePipe{}))
	names := reg.Names()
	for _, n := range ops.CoreWorkerOpNames {
		if !slices.Contains(names, n) {
			t.Errorf("%s has no handler", n)
		}
	}
	if len(names) != len(ops.CoreWorkerOpNames) {
		t.Errorf("%d handlers, want %d", len(names), len(ops.CoreWorkerOpNames))
	}
	// It replaces RegisterWorkerOps: both together collide.
	if err := RegisterWorkerOps(reg, rt); err == nil {
		t.Error("the pipe ops were registered twice")
	}
	if err := RegisterPipeOps(NewRegistry(ops.Default), nil); err == nil {
		t.Error("a nil service was accepted")
	}
}

func TestPipeSendGoesToTheHubPipeNotToTheWorker(t *testing.T) {
	wid := ops.NewUUIDv7()
	svc := &fakePipe{sendRes: ops.WriteResult{Outcome: ops.OutcomeDelivered}}
	rt := &woRouter{}
	alice := authz.NewSessionPrincipal("alice")
	s := newWOServer(t, ops.Default, alice, woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOpsWithPipe(r, rt, svc)) })

	rid := ops.NewUUIDv7()
	w := s.do("POST", "/api/v1/ops/pipe_send", `{"request_id":"`+rid+`","target":{"worker_id":"`+wid+`","session_name":"minion-1"},"text":"hello"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("body = %s (%v)", w.Body, err)
	}
	if len(svc.sends) != 1 || svc.sends[0].Text != "hello" || svc.sends[0].RequestID != rid || svc.sends[0].Target.SessionName != "minion-1" || svc.who[0].UserID != "alice" {
		t.Fatalf("service got %+v for %+v", svc.sends, svc.who)
	}
	if len(rt.calls) != 0 {
		t.Fatalf("pipe_send was also forwarded to the Worker by the plain router: %+v", rt.calls)
	}
}

func TestPipeSendIsAuthorisedBeforeTheService(t *testing.T) {
	wid := ops.NewUUIDv7()
	svc := &fakePipe{}
	s := newWOServer(t, ops.Default, authz.NewSessionPrincipal("bob"), woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterPipeOps(r, svc)) })
	w := s.do("POST", "/api/v1/ops/pipe_send", `{"request_id":"`+ops.NewUUIDv7()+`","target":{"worker_id":"`+wid+`","session_name":"minion-1"},"text":"hello"}`)
	if got := woErrCode(t, w); got != proto.CodeNotFound {
		t.Fatalf("code %q", got)
	}
	if len(svc.sends) != 0 {
		t.Fatal("the service was reached by a caller who may not message the session")
	}
}

func TestPipeReadReturnsWithheldTurnsAndTheReason(t *testing.T) {
	wid := ops.NewUUIDv7()
	svc := &fakePipe{readRes: workers.PipeReadView{Turns: []ops.PipeTurn{}, Withheld: 2, Reason: workers.ReasonUntrustedPeer, Notice: "withheld"}}
	key := authz.NewAPIKeyPrincipal("alice", scopeAllActions{}, "")
	s := newWOServer(t, ops.Default, key, woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterPipeOps(r, svc)) })
	w := s.do("POST", "/api/v1/ops/pipe_read", `{"target":{"worker_id":"`+wid+`","session_name":"minion-1"},"limit":5}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["withheld"] != float64(2) || body["reason"] != "untrusted_peer" {
		t.Fatalf("body = %s", w.Body)
	}
	if turns, ok := body["turns"].([]any); !ok || len(turns) != 0 {
		t.Fatalf("turns = %v, want an empty array", body["turns"])
	}
	if svc.who[0].Kind != authz.KindAPIKey || svc.reads[0].Limit != 5 {
		t.Fatalf("the service got principal %q and %+v", svc.who[0].Kind, svc.reads)
	}
}

func TestPipeWaitStreamsTurnsAndMapsErrors(t *testing.T) {
	wid := ops.NewUUIDv7()
	tg := `{"target":{"worker_id":"` + wid + `","session_name":"minion-1"},"timeout_ms":10}`
	svc := &fakePipe{waitTurn: []ops.PipeTurn{{TurnID: ops.NewUUIDv7(), Session: "minion-1", Direction: ops.PipeReceived, Text: "reply"}}}
	s := newWOServer(t, ops.Default, authz.NewSessionPrincipal("alice"), woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterPipeOps(r, svc)) })
	w := s.do("POST", "/api/v1/ops/pipe_wait", tg)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"reply"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	svc2 := &fakePipe{err: proto.NewError(proto.CodeUntrustedPeer, "no")}
	s2 := newWOServer(t, ops.Default, authz.NewSessionPrincipal("alice"), woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterPipeOps(r, svc2)) })
	w = s2.do("POST", "/api/v1/ops/pipe_wait", tg)
	if got := woErrCode(t, w); got != proto.CodeUntrustedPeer {
		t.Fatalf("code %q", got)
	}
}

// scopeAllActions grants every non-admin action.
type scopeAllActions struct{}

func (scopeAllActions) Actions(authz.ResolvedResource) []ops.Action {
	return []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage}
}

// MCP in Hub mode is a REST client with the user's API key (AD-22): its
// pipe_send tool call is exactly this request. The Hub pipe service, not the
// plain router, answers it, as the key's principal.
func TestPipeSendWithAnAPIKeyAsMCPOverTheHubReachesTheHubPipe(t *testing.T) {
	wid := ops.NewUUIDv7()
	svc := &fakePipe{sendRes: ops.WriteResult{Outcome: ops.OutcomeDelivered}}
	rt := &woRouter{}
	key := authz.NewAPIKeyPrincipal("alice", scopeAllActions{}, "")
	s := newWOServer(t, ops.Default, key, woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOpsWithPipe(r, rt, svc)) })
	w := s.do("POST", "/api/v1/ops/pipe_send", `{"request_id":"`+ops.NewUUIDv7()+`","target":{"worker_id":"`+wid+`","session_name":"minion-1"},"text":"from a model"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(svc.sends) != 1 || svc.sends[0].Text != "from a model" || svc.who[0].Kind != authz.KindAPIKey || len(rt.calls) != 0 {
		t.Fatalf("service got %+v for %+v; plain router got %d calls", svc.sends, svc.who, len(rt.calls))
	}
}

// TestPipeSendIgnoresAnInternalMessageIDFromTheCaller: message_id is the Hub's
// own member of the Worker request, not an argument of the public API. A
// caller that sends one (in the body or the query) does not reach the service
// with it.
func TestPipeSendIgnoresAnInternalMessageIDFromTheCaller(t *testing.T) {
	wid := ops.NewUUIDv7()
	svc := &fakePipe{sendRes: ops.WriteResult{Outcome: ops.OutcomeDelivered}}
	alice := authz.NewSessionPrincipal("alice")
	s := newWOServer(t, ops.Default, alice, woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOpsWithPipe(r, &woRouter{}, svc)) })
	w := s.do("POST", "/api/v1/ops/pipe_send", `{"request_id":"`+ops.NewUUIDv7()+`","message_id":99,"target":{"worker_id":"`+wid+`","session_name":"minion-1"},"text":"hello"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(svc.sends) != 1 || svc.sends[0].MessageID != 0 {
		t.Fatalf("the service got %+v, want message_id 0", svc.sends)
	}
}
