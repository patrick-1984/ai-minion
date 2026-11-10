package web

// The composer's two buttons post an `enter` value (Send: 1, "Paste without Enter": 0). The route turns exactly enter=1 into ONE
// send_text call with submit=true; anything else is submit=false. These tests pin that contract at the route, with the values the
// browser (htmx, from the clicked button) really posts.

import (
	"net/url"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestSendTextSubmitFlagFollowsTheClickedButtonExactly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		form   url.Values
		submit bool
	}{
		{"Send (enter=1)", url.Values{"text": {"ls"}, "enter": {"1"}}, true},
		{"Paste without Enter (enter=0)", url.Values{"text": {"ls"}, "enter": {"0"}}, false},
		{"no submitter at all", url.Values{"text": {"ls"}}, false},
		{"an unexpected value", url.Values{"text": {"ls"}, "enter": {"true"}}, false},
	} {
		s, f := sessionOps(t)
		rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, tc.form)
		mustHave(t, rec.Body.String(), "<strong>Text:</strong> Delivered.")
		if n := len(f.called(ops.OpSendText)); n != 1 {
			t.Fatalf("%s: %d send_text calls, want exactly one (the text and its Enter are one call)", tc.name, n)
		}
		if n := len(f.called(ops.OpSendKeys)); n != 0 {
			t.Errorf("%s: a separate key call was made (%d): Enter must never be a second request", tc.name, n)
		}
		if a := argsOf(f, ops.OpSendText, 0); (a["submit"] == true) != tc.submit || a["text"] != "ls" {
			t.Errorf("%s: args %+v, want submit=%v", tc.name, a, tc.submit)
		}
	}
	// Two enter values in one request (a hand-made POST) are still ONE call and the first value decides: the page never makes two.
	s, f := sessionOps(t)
	post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"ls"}, "enter": {"1", "1"}})
	if n := len(f.called(ops.OpSendText)); n != 1 {
		t.Errorf("duplicate enter values made %d calls", n)
	}
}

// An empty text is refused before any call: a blank box is never a remote Enter (the Send Enter key is its own action).
func TestBlankTextIsNeverARemoteEnter(t *testing.T) {
	s, f := sessionOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {""}, "enter": {"1"}})
	mustHave(t, rec.Body.String(), "Type some text first.")
	if n := len(f.called(ops.OpSendText)) + len(f.called(ops.OpSendKeys)); n != 0 {
		t.Errorf("an empty text made %d calls", n)
	}
}

// A whitespace-only primary Send (enter=1) is refused by the route before any native call, exactly like the box's Enter; an explicit
// paste without Enter (enter=0) keeps its whitespace, and a forced retry of a blank text is refused too.
func TestWhitespaceOnlyEnterSendIsRefusedBeforeAnyNativeCall(t *testing.T) {
	for _, blank := range []string{" ", "   ", "\t", "\n", "\r\n \r\n", "\u00a0", "\ufeff"} {
		for _, extra := range []url.Values{{}, {"force": {"1"}}} {
			s, f := sessionOps(t)
			form := url.Values{"text": {blank}, "enter": {"1"}}
			for k, v := range extra {
				form[k] = v
			}
			rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, form)
			mustHave(t, rec.Body.String(), "Type some text first.")
			if n := len(f.called(ops.OpSendText)) + len(f.called(ops.OpSendKeys)); n != 0 {
				t.Errorf("blank %q (%v) made %d native calls", blank, extra, n)
			}
		}
	}
	s, f := sessionOps(t)
	post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"   "}, "enter": {"0"}})
	if n := len(f.called(ops.OpSendText)); n != 1 {
		t.Fatalf("explicit paste-only whitespace made %d calls, want 1", n)
	}
	if a := argsOf(f, ops.OpSendText, 0); a["submit"] == true || a["text"] != "   " {
		t.Errorf("paste-only args %+v", a)
	}
	s, f = sessionOps(t)
	post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {" ls "}, "enter": {"1"}})
	if a := argsOf(f, ops.OpSendText, 0); a["submit"] != true || a["text"] != " ls " {
		t.Errorf("non-blank text must be sent untrimmed with submit=true: %+v", a)
	}
}
