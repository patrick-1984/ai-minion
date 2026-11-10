package web

// The markup of the settings page, parsed (x/net/html), for what a string search cannot say: every setting is one
// form that owns exactly its own controls, every label, description and result target is unique and resolves, and
// the locked and secret rows stay inert and empty. Layout (row height, alignment, 320px) is a Shared browser check.

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func settingForms(t *testing.T, body string) (*html.Node, []*html.Node) {
	t.Helper()
	doc := parse(t, body)
	forms := find(doc, func(n *html.Node) bool {
		return n.Data == "form" && strings.HasPrefix(attr(n, "hx-post"), "/admin/settings/")
	})
	return doc, forms
}

func TestSettingsEachRowIsOneFormOwningOnlyItsControls(t *testing.T) {
	s, _ := settingsServer(t)
	body := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader).Body.String()
	doc, forms := settingForms(t, body)
	list := sampleSettings().Settings
	if len(forms) != len(list) {
		t.Fatalf("%d setting forms for %d settings", len(forms), len(list))
	}
	ids := map[string]int{}
	for _, n := range find(doc, func(n *html.Node) bool { return hasAttr(n, "id") }) {
		ids[attr(n, "id")]++
	}
	for i, st := range list {
		f := forms[i]
		if got := attr(f, "hx-post"); got != "/admin/settings/"+st.Key {
			t.Errorf("form %d posts to %q, want the key %q", i, got, st.Key)
		}
		if attr(f, "method") != "post" || attr(f, "hx-target") != "#result-"+st.Key {
			t.Errorf("%s: form attributes %v", st.Key, f.Attr)
		}
		if hasAncestorOrSelf(f.Parent, func(n *html.Node) bool { return n.Data == "form" }) ||
			len(find(f, func(n *html.Node) bool { return n != f && n.Data == "form" })) != 0 {
			t.Errorf("%s: forms are nested", st.Key)
		}
		// Every control of the row sits inside its form, so it cannot post for a neighbour; nothing carries a form= redirect.
		controls := find(f, func(n *html.Node) bool { return n.Data == "input" || n.Data == "select" || n.Data == "button" })
		for _, c := range controls {
			if hasAttr(c, "form") {
				t.Errorf("%s: %s re-assigns its form", st.Key, c.Data)
			}
		}
		// The label names the value control; the description and meta ids it points to exist exactly once.
		label := find(f, func(n *html.Node) bool { return n.Data == "label" })
		if len(label) != 1 || attr(label[0], "for") != "v-"+st.Key || ids["v-"+st.Key] != 1 {
			t.Errorf("%s: label/control pairing broken (ids %d)", st.Key, ids["v-"+st.Key])
		}
		ctl := find(f, func(n *html.Node) bool { return attr(n, "id") == "v-"+st.Key })
		if len(ctl) != 1 {
			t.Fatalf("%s: %d value controls", st.Key, len(ctl))
		}
		for _, id := range strings.Fields(attr(ctl[0], "aria-describedby")) {
			if ids[id] != 1 {
				t.Errorf("%s: aria-describedby %q resolves to %d elements", st.Key, id, ids[id])
			}
		}
		if ids["result-"+st.Key] != 1 {
			t.Errorf("%s: %d result targets", st.Key, ids["result-"+st.Key])
		}
		// Only an editable value control has a name; the Save and the default/remove buttons carry the set/unset action.
		named := ctl[0].Data != "input" || hasAttr(ctl[0], "name")
		if named == hasAttr(ctl[0], "disabled") {
			t.Errorf("%s: control named=%v disabled=%v", st.Key, named, hasAttr(ctl[0], "disabled"))
		}
		actions := map[string]string{}
		for _, b := range find(f, func(n *html.Node) bool { return n.Data == "button" }) {
			actions[attr(b, "value")] = text(b)
		}
		switch {
		case st.ReadOnly:
			if len(actions) != 0 {
				t.Errorf("%s: a locked row offers %v", st.Key, actions)
			}
		case st.Secret:
			if actions["set"] != "Save" || actions["unset"] != "Remove" {
				t.Errorf("%s: secret actions %v", st.Key, actions)
			}
		default:
			if actions["set"] != "Save" || actions["unset"] != "Use the default" {
				t.Errorf("%s: actions %v", st.Key, actions)
			}
		}
	}
}

// A secret row posts the "secret" marker with an empty password field, never its stored value; the stored value
// of every secret, locked or not, appears nowhere on the page.
func TestSettingsSecretRowsPostAnEmptyPasswordAndRenderNoValue(t *testing.T) {
	s, _ := settingsServer(t)
	body := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader).Body.String()
	mustNotHave(t, body, "MASKED-VALUE")
	_, forms := settingForms(t, body)
	for _, f := range forms {
		key := strings.TrimPrefix(attr(f, "hx-post"), "/admin/settings/")
		for _, st := range sampleSettings().Settings {
			if st.Key != key || !st.Secret {
				continue
			}
			for _, in := range find(f, func(n *html.Node) bool { return n.Data == "input" && attr(n, "type") == "password" }) {
				if v := attr(in, "value"); v != "" {
					t.Errorf("%s: password input renders value %q", key, v)
				}
			}
			marker := find(f, func(n *html.Node) bool { return n.Data == "input" && attr(n, "name") == "secret" })
			if st.ReadOnly == (len(marker) != 0) {
				t.Errorf("%s: secret marker present=%v, locked=%v", key, len(marker) != 0, st.ReadOnly)
			}
		}
	}
}
