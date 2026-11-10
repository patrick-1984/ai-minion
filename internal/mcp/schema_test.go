package mcp

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// jsonMembers returns the JSON member names of the struct type t, read from
// its json tags (omitempty is ignored, "-" skips the field).
func jsonMembers(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestTargetSchemaCoversEveryTargetMember guards the hand-written ops.Target
// schema, which closes additionalProperties: a field added to ops.Target must
// also be added there, or MCP would reject it.
func TestTargetSchemaCoversEveryTargetMember(t *testing.T) {
	override := typeSchemas()[reflect.TypeFor[ops.Target]()]
	if override == nil {
		t.Fatal("no override for ops.Target")
	}
	var got []string
	for name := range override.Properties {
		got = append(got, name)
	}
	slices.Sort(got)
	if want := jsonMembers(reflect.TypeFor[ops.Target]()); !slices.Equal(got, want) {
		t.Fatalf("Target schema properties = %v, ops.Target JSON members = %v", got, want)
	}
}

// TestPipeSendDescribesItsOwnSubmission: pipe_send always presses Enter and has
// no submit argument; send_text adds Enter only when submit is true. The two
// descriptions of the member "text" must say so, and must not contradict the
// behaviour (an MCP caller that follows the advertised default to stage a text
// must not submit it).
func TestPipeSendDescribesItsOwnSubmission(t *testing.T) {
	props := func(name string) (map[string]string, bool) {
		op, ok := ops.Lookup(name)
		if !ok {
			t.Fatalf("no op %s", name)
		}
		s, err := inputSchema(op)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for k, p := range s.Properties {
			out[k] = p.Description
		}
		_, hasSubmit := s.Properties["submit"]
		return out, hasSubmit
	}
	pipe, pipeSubmit := props(ops.OpPipeSend)
	send, sendSubmit := props(ops.OpSendText)
	if pipeSubmit || !sendSubmit {
		t.Fatalf("submit argument: pipe_send %v, send_text %v", pipeSubmit, sendSubmit)
	}
	if d := pipe["text"]; !strings.Contains(d, "always presses Enter") || strings.Contains(d, "not submitted") {
		t.Errorf("pipe_send text description: %q", d)
	}
	if d := send["text"]; !strings.Contains(d, "No Enter is added unless submit is true") {
		t.Errorf("send_text text description: %q", d)
	}
	if _, leaked := pipe["message_id"]; leaked {
		t.Error("message_id is a public pipe_send argument")
	}
}
