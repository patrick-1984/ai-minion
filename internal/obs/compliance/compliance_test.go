package compliance

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type collect struct {
	bodies []string
	attrs  [][]KV
}

func (c *collect) EmitEvent(_ string, _ time.Time, body string, attrs []KV) {
	c.bodies = append(c.bodies, body)
	c.attrs = append(c.attrs, attrs)
}

func TestChunksKeepRunesWholeAndLoseNothing(t *testing.T) {
	s := strings.Repeat("aé€😀", 40)
	got := Chunks(s, 7)
	if strings.Join(got, "") != s {
		t.Fatal("chunks do not add up to the text")
	}
	for _, c := range got {
		if len(c) > 7 || !utf8.ValidString(c) {
			t.Fatalf("bad chunk %q", c)
		}
	}
}

func TestEmitMasksBeforeSplittingAndGatesByMode(t *testing.T) {
	key := "mn_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"
	// The key straddles the first chunk boundary.
	text := strings.Repeat("x", ChunkBytes-10) + " " + key + " tail"
	c := &collect{}
	m := New(func() Sink { return c })
	m.Emit(Actions, EventInput, text) // mode is off
	if len(c.bodies) != 0 {
		t.Fatal("recorded while off")
	}
	m.SetMode(Full)
	m.Emit(Full, EventInput, text)
	all := strings.Join(c.bodies, "")
	if strings.Contains(all, key) || strings.Contains(all, key[:20]) || !strings.HasSuffix(all, " tail") {
		t.Fatalf("not masked before the split: ...%q", all[len(all)-80:])
	}
	if len(c.bodies) < 2 {
		t.Fatalf("expected a split, got %d chunks", len(c.bodies))
	}
	last := c.attrs[len(c.attrs)-1]
	var idx, cnt int64
	for _, a := range last {
		switch a.Key {
		case "minion.chunk.index":
			idx = a.Value.(int64)
		case "minion.chunk.count":
			cnt = a.Value.(int64)
		}
	}
	if cnt != int64(len(c.bodies)) || idx != cnt-1 {
		t.Fatalf("chunk index/count = %d/%d for %d chunks", idx, cnt, len(c.bodies))
	}
	m.SetMode(Output)
	n := len(c.bodies)
	m.Emit(Full, EventInput, "more")
	if len(c.bodies) != n {
		t.Fatal("full event recorded at output level")
	}
}

func TestNoSinkNeverPanicsOrBlocks(t *testing.T) {
	var m *Monitor
	m.Emit(Actions, EventAction, "x")
	m2 := New(func() Sink { return nil })
	m2.SetMode(Full)
	m2.Emit(Actions, EventAction, "x")
	if !strings.Contains(m2.Status(), "otel_level") {
		t.Fatal("status does not say why nothing is sent")
	}
}

// A configuration change between the mode check and the sink lookup drops the
// record: it is never sent to the replacement sink.
func TestEmitDropsWhenTheConfigurationChangesMidway(t *testing.T) {
	c := &collect{}
	var m *Monitor
	m = New(func() Sink {
		m.SetMode(Off) // an admin turns monitoring off and retargets, right now
		return c
	})
	m.SetMode(Full)
	m.Emit(Full, EventInput, "typed text")
	if len(c.bodies) != 0 {
		t.Fatalf("sent %v to the sink of a configuration that was gone", c.bodies)
	}
}

// Every string attribute is masked and bounded; a secret split by line breaks
// is masked whole.
func TestAttributesAreMaskedAndBoundedAndWrapsAreJoined(t *testing.T) {
	key := "mn_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"
	c := &collect{}
	m := New(func() Sink { return c })
	m.SetMode(Full)
	m.Emit(Actions, EventAction, "", KV{"minion.session", "name-" + key}, KV{"minion.outcome", strings.Repeat("o", 5000)})
	for _, a := range c.attrs[0] {
		v := a.Value.(string)
		if strings.Contains(v, key[:12]) || len(v) > MaxAttrBytes {
			t.Fatalf("attribute %s = %d bytes: %.60q", a.Key, len(v), v)
		}
	}
	m.Emit(Output, EventOutput, "row one "+key[:25]+"\n"+key[25:]+"\nrow three")
	if b := c.bodies[1]; strings.Contains(b, key[:12]) || strings.Contains(b, key[25:37]) {
		t.Fatalf("wrapped secret not masked: %q", b)
	}
}

// A join token that wraps before its embedded secret: the continuation is
// masked with the rest.
func TestWrappedJoinTokenIsMaskedWhole(t *testing.T) {
	c := &collect{}
	m := New(func() Sink { return c })
	m.SetMode(Output)
	m.Emit(Output, EventOutput, "$ minion join mnj1-eyJhIjoi\nSECRETPAYLOADxyz123\n$")
	if b := c.bodies[0]; strings.Contains(b, "SECRETPAYLOAD") || strings.Contains(b, "eyJhIjoi") {
		t.Fatalf("not masked: %q", b)
	}
}

// An intact credential at the start of a row that follows a letter, and a
// wrapped one, in the same observation: both are masked.
func TestIntactAndWrappedCredentialsTogether(t *testing.T) {
	key := "mn_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"
	c := &collect{}
	m := New(func() Sink { return c })
	m.SetMode(Output)
	m.Emit(Output, EventOutput, "prompt$ abc\n"+key+"\nrow mn_"+key[3:30]+"\n"+key[30:]+"\nend")
	b := c.bodies[0]
	if strings.Contains(b, key[3:20]) || strings.Contains(b, key[30:45]) || !strings.Contains(b, "end") {
		t.Fatalf("not masked: %q", b)
	}
}
