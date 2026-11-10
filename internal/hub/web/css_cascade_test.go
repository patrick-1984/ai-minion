package web

// A small CSS cascade over static/minion.css, for the questions a browser answers
// and a string search cannot: which declaration wins for a given element at a
// given window width. It exists because the first workbench build had two bugs
// that no search for a class name finds: ".menu-btn { display: none }" lost to the
// later ".btn { display: inline-flex }" of the same specificity (the Menu button
// stood beside the permanent rail), and a later rule put the narrow session pane
// back into the normal flow below the page. It handles what minion.css uses:
// type, class, id and attribute selectors, :not(), :where(), :root, descendant
// and child combinators, @media (min-width / max-width), !important. A selector
// with another pseudo-class or a pseudo-element (:hover, :disabled, ::before)
// describes a state or a part, not the element at rest, and never matches here.
// It is not a browser: layout, inheritance and the real pages stay with the
// Shared browser checks.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cssEl is an element at rest: a tag, its classes and its attributes.
type cssEl struct {
	tag     string
	classes []string
	attrs   map[string]string
}

func el(tag string, classes string, attrs ...string) cssEl {
	e := cssEl{tag: tag, classes: strings.Fields(classes), attrs: map[string]string{}}
	for i := 0; i+1 < len(attrs); i += 2 {
		e.attrs[attrs[i]] = attrs[i+1]
	}
	return e
}

type cssDecl struct {
	prop, val string
	important bool
}

type cssRule struct {
	sel        string
	decls      []cssDecl
	minW, maxW int // media bounds; maxW 0 means none
	order      int
}

var cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// parseCSS reads the rules of a style sheet, with @media (min-width|max-width) blocks one level deep.
func parseCSS(t *testing.T, src string) []cssRule {
	t.Helper()
	src = cssComment.ReplaceAllString(src, "")
	var rules []cssRule
	var walk func(s string, minW, maxW int)
	walk = func(s string, minW, maxW int) {
		for {
			s = strings.TrimSpace(s)
			if s == "" {
				return
			}
			open := strings.Index(s, "{")
			if open < 0 {
				return
			}
			head := strings.TrimSpace(s[:open])
			depth, end := 1, open+1
			for ; end < len(s) && depth > 0; end++ {
				switch s[end] {
				case '{':
					depth++
				case '}':
					depth--
				}
			}
			body := s[open+1 : end-1]
			s = s[end:]
			if strings.HasPrefix(head, "@media") {
				mn, mx := minW, maxW
				if m := regexp.MustCompile(`^@media\s*\(\s*(min|max)-width:\s*(\d+)px\s*\)\s*$`).FindStringSubmatch(head); m != nil {
					v, _ := strconv.Atoi(m[2])
					if m[1] == "min" {
						mn = v
					} else {
						mx = v
					}
					walk(body, mn, mx)
				} // another media query (colour scheme, motion, print) is not a width and is skipped
				continue
			}
			if strings.HasPrefix(head, "@") {
				continue
			}
			var decls []cssDecl
			for _, d := range splitTop(body, ';') {
				k, v, ok := strings.Cut(d, ":")
				if !ok {
					continue
				}
				v = strings.TrimSpace(v)
				imp := strings.HasSuffix(v, "!important")
				v = strings.TrimSpace(strings.TrimSuffix(v, "!important"))
				decls = append(decls, cssDecl{strings.TrimSpace(k), v, imp})
			}
			for _, sel := range splitTop(head, ',') {
				rules = append(rules, cssRule{sel: strings.TrimSpace(sel), decls: decls, minW: minW, maxW: maxW, order: len(rules)})
			}
		}
	}
	walk(src, 0, 0)
	return rules
}

// splitTop splits s at sep outside parentheses, brackets and quotes.
func splitTop(s string, sep byte) []string {
	var out []string
	depth, start := 0, 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if strings.TrimSpace(s[start:]) != "" {
		out = append(out, s[start:])
	}
	return out
}

// specificity packs (ids, classes+attributes+pseudo-classes, types) into one number.
type spec [3]int

func (a spec) n() int { return a[0]*1_000_000 + a[1]*1_000 + a[2] }

// matchCompound reports whether the compound selector (no combinators) matches e and its specificity.
func matchCompound(sel string, e cssEl, root bool) (bool, spec) {
	var sp spec
	i := 0
	for i < len(sel) {
		c := sel[i]
		switch {
		case c == '*':
			i++
		case c == '.':
			j := i + 1
			for j < len(sel) && isIdent(sel[j]) {
				j++
			}
			ok := false
			for _, k := range e.classes {
				ok = ok || k == sel[i+1:j]
			}
			if !ok {
				return false, sp
			}
			sp[1]++
			i = j
		case c == '#':
			j := i + 1
			for j < len(sel) && isIdent(sel[j]) {
				j++
			}
			if id, ok := e.attrs["id"]; !ok || id != sel[i+1:j] {
				return false, sp // the element has no such id (most elements of these tests have none)
			}
			sp[0]++
			i = j
		case c == '[':
			j := strings.Index(sel[i:], "]") + i
			if j < i {
				return false, sp
			}
			if !matchAttr(sel[i+1:j], e) {
				return false, sp
			}
			sp[1]++
			i = j + 1
		case c == ':':
			if strings.HasPrefix(sel[i:], "::") {
				return false, sp
			}
			j := i + 1
			for j < len(sel) && (isIdent(sel[j])) {
				j++
			}
			name := sel[i+1 : j]
			args := ""
			if j < len(sel) && sel[j] == '(' {
				depth, k := 1, j+1
				for ; k < len(sel) && depth > 0; k++ {
					switch sel[k] {
					case '(':
						depth++
					case ')':
						depth--
					}
				}
				args = sel[j+1 : k-1]
				j = k
			}
			switch name {
			case "root":
				if !root {
					return false, sp
				}
				sp[1]++
			case "where", "not", "is":
				any, best := false, spec{}
				for _, a := range splitTop(args, ',') {
					ok, s := matchCompound(strings.TrimSpace(a), e, root)
					any = any || ok
					if s.n() > best.n() {
						best = s
					}
				}
				if name == "not" {
					if any {
						return false, sp
					}
					sp[0] += best[0]
					sp[1] += best[1]
					sp[2] += best[2]
				} else {
					if !any {
						return false, sp
					}
					if name == "is" {
						sp[0] += best[0]
						sp[1] += best[1]
						sp[2] += best[2]
					}
				}
			default:
				return false, sp // a state or a position: not the element at rest
			}
			i = j
		case isIdent(c):
			j := i
			for j < len(sel) && isIdent(sel[j]) {
				j++
			}
			if sel[i:j] != e.tag {
				return false, sp
			}
			sp[2]++
			i = j
		default:
			return false, sp
		}
	}
	return true, sp
}

func isIdent(c byte) bool {
	return c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func matchAttr(a string, e cssEl) bool {
	k, v, has := strings.Cut(a, "=")
	k = strings.TrimSpace(k)
	got, present := e.attrs[k]
	if !has {
		return present
	}
	v = strings.Trim(strings.TrimSpace(v), `"'`)
	return present && got == v
}

// matchSelector matches a complex selector against e, whose ancestors are given nearest first.
func matchSelector(sel string, e cssEl, ancestors []cssEl) (bool, spec) {
	parts := splitCombinators(sel)
	if len(parts) == 0 {
		return false, spec{}
	}
	ok, sp := matchCompound(parts[len(parts)-1], e, e.tag == "html")
	if !ok {
		return false, sp
	}
	at := 0
	for k := len(parts) - 2; k >= 0; k-- {
		found := false
		for ; at < len(ancestors); at++ {
			if ok, s := matchCompound(parts[k], ancestors[at], ancestors[at].tag == "html"); ok {
				sp[0] += s[0]
				sp[1] += s[1]
				sp[2] += s[2]
				found = true
				at++
				break
			}
		}
		if !found {
			return false, sp
		}
	}
	return true, sp
}

// valueAt returns the value of prop for e at a window width of w px, as the cascade decides it ("" when no rule sets it).
func valueAt(rules []cssRule, prop string, e cssEl, ancestors []cssEl, w int) string {
	type win struct {
		imp   bool
		sp    int
		order int
		val   string
	}
	var best *win
	for _, r := range rules {
		if (r.minW != 0 && w < r.minW) || (r.maxW != 0 && w > r.maxW) {
			continue
		}
		ok, sp := matchSelector(r.sel, e, ancestors)
		if !ok {
			continue
		}
		for _, d := range r.decls {
			if d.prop != prop {
				continue
			}
			c := win{d.important, sp.n(), r.order, d.val}
			if best == nil || (c.imp != best.imp && c.imp) ||
				(c.imp == best.imp && (c.sp > best.sp || (c.sp == best.sp && c.order > best.order))) {
				cc := c
				best = &cc
			}
		}
	}
	if best == nil {
		return ""
	}
	return best.val
}

func loadSheet(t *testing.T) []cssRule {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("static", "minion.css"))
	if err != nil {
		t.Fatal(err)
	}
	return parseCSS(t, string(b))
}

var scripted = []cssEl{el("html", "js")}

// The cascade evaluator itself: later rules of equal specificity win, a more
// specific rule wins over a later one, and the media bounds are inclusive.
func TestCascadeEvaluator(t *testing.T) {
	rules := parseCSS(t, `
.a { display: none; }
.b, button { display: flex; }
.a.b { color: red; }
.b { color: blue; }
@media (max-width: 959px) { .a { display: block; } }
@media (min-width: 960px) { .x { display: grid; } }
.js .c { display: inline; } .c:hover { display: none; } .c::before { display: none; }
input:where(:not([type="radio"], [type="hidden"])) { border: 1px solid red; }
input[type="number"] { border: 0; }
`)
	both := el("div", "a b")
	for _, c := range []struct {
		prop string
		e    cssEl
		anc  []cssEl
		w    int
		want string
	}{
		{"display", both, nil, 1000, "flex"}, // .b is later than .a, same specificity
		{"color", both, nil, 1000, "red"},    // .a.b is more specific than the later .b
		{"display", both, nil, 959, "block"}, // the media rule is the last one
		{"display", both, nil, 960, "flex"},  // outside the media bound
		{"display", el("div", "x"), nil, 960, "grid"},
		{"display", el("div", "x"), nil, 959, ""},
		{"display", el("div", "c"), nil, 500, ""},            // needs the .js ancestor
		{"display", el("div", "c"), scripted, 500, "inline"}, // :hover and ::before never match
		{"border", el("input", "", "type", "text"), nil, 500, "1px solid red"},
		{"border", el("input", "", "type", "radio"), nil, 500, ""},
		{"border", el("input", ""), nil, 500, "1px solid red"},       // no type attribute: a text field
		{"border", el("input", "", "type", "number"), nil, 500, "0"}, // :where() adds no specificity
	} {
		if got := valueAt(rules, c.prop, c.e, c.anc, c.w); got != c.want {
			t.Errorf("%s of %+v at %dpx = %q, want %q", c.prop, c.e, c.w, got, c.want)
		}
	}
}

// The Menu button belongs to the narrow layouts only: with the left rail
// permanent (960px and up) it must not be shown, and the Close button of the
// rail is a narrow control as well.
func TestMenuButtonShowsOnlyWhereTheRailIsAnOverlay(t *testing.T) {
	rules := loadSheet(t)
	menu := el("button", "btn btn-secondary menu-btn", "type", "button")
	closeBtn := el("button", "rail-close", "type", "button")
	for _, w := range []int{1920, 1280, 960} {
		if d := valueAt(rules, "display", menu, scripted, w); d != "none" {
			t.Errorf("the Menu button has display %q at %dpx, want none (the rail is permanent)", d, w)
		}
		if d := valueAt(rules, "display", closeBtn, scripted, w); d != "none" {
			t.Errorf("the rail Close button has display %q at %dpx, want none", d, w)
		}
	}
	for _, w := range []int{959, 640, 320} {
		if d := valueAt(rules, "display", menu, scripted, w); d == "none" || d == "" {
			t.Errorf("the Menu button has display %q at %dpx, want it shown", d, w)
		}
		if d := valueAt(rules, "display", closeBtn, scripted, w); d == "none" || d == "" {
			t.Errorf("the rail Close button has display %q at %dpx, want it shown", d, w)
		}
	}
}

// An open session pane is docked in the grid from 1280px and a modal drawer (fixed,
// with a scrim) below, down to 320px: never back in the normal flow below the page,
// where the person who chose Details sees nothing happen.
func TestOpenSessionPaneIsADrawerBelowTheDockedWidth(t *testing.T) {
	rules := loadSheet(t)
	pane := el("aside", "session-pane open")
	head := el("div", "session-pane-head")
	scrim := el("div", "pane-scrim open")
	for _, w := range []int{320, 375, 768, 959, 960, 1279} {
		if p := valueAt(rules, "position", pane, scripted, w); p != "fixed" {
			t.Errorf("the open pane has position %q at %dpx, want fixed (a drawer)", p, w)
		}
		if d := valueAt(rules, "display", pane, scripted, w); d == "none" {
			t.Errorf("the open pane is not displayed at %dpx", w)
		}
		if d := valueAt(rules, "display", scrim, scripted, w); d != "block" {
			t.Errorf("the scrim of the open drawer has display %q at %dpx, want block", d, w)
		}
		if p := valueAt(rules, "position", scrim, scripted, w); p != "fixed" {
			t.Errorf("the scrim has position %q at %dpx, want fixed", p, w)
		}
		vz, e1 := strconv.Atoi(valueAt(rules, "z-index", scrim, scripted, w))
		pz, e2 := strconv.Atoi(valueAt(rules, "z-index", pane, scripted, w))
		if e1 != nil || e2 != nil || pz <= vz {
			t.Errorf("at %dpx the drawer (z-index %d) must stand above its scrim (%d)", w, pz, vz)
		}
		if p := valueAt(rules, "position", head, append([]cssEl{pane}, scripted...), w); p != "sticky" {
			t.Errorf("the drawer head has position %q at %dpx, want sticky so Close stays in view", p, w)
		}
	}
	for _, w := range []int{1280, 1920} {
		if p := valueAt(rules, "position", pane, scripted, w); p == "fixed" || p == "absolute" {
			t.Errorf("the docked pane has position %q at %dpx, want it in the grid", p, w)
		}
		if d := valueAt(rules, "display", scrim, scripted, w); d != "none" {
			t.Errorf("the scrim has display %q at %dpx, want none while the pane is docked", d, w)
		}
	}
	// Closed with a script: not displayed; without a script (no .js): shown in the flow.
	if d := valueAt(rules, "display", el("aside", "session-pane"), scripted, 320); d != "none" {
		t.Errorf("a closed pane has display %q, want none", d)
	}
	if d := valueAt(rules, "display", el("aside", "session-pane"), nil, 320); d == "none" {
		t.Error("without a script the pane must stay visible in the flow")
	}
}

// A fixed drawer anchored at right:0 must fit the LAYOUT viewport, which is the window
// less a classic scrollbar gutter (15 to 17px; a 320px window has a 305px layout
// viewport). A width in vw counts the gutter: min(352px, 100vw) is 320px in a 305px
// viewport and the drawer's left edge lands at x=-15, clipping its content (seen in the
// Shared browser at 320x800). A percentage of a fixed element refers to the layout
// viewport. This evaluates the cascaded width of the real rule; it models the
// geometry, it is not a browser (the real bounding box is checked there).
func TestOpenSessionPaneFitsTheLayoutViewportWithAScrollbarGutter(t *testing.T) {
	rules := loadSheet(t)
	pane := el("aside", "session-pane open")
	declared := regexp.MustCompile(`--pane-w:\s*(\d+)px`).FindStringSubmatch(loadCSS(t))
	if declared == nil {
		t.Fatal("no --pane-w token")
	}
	paneWidth, _ := strconv.Atoi(declared[1])
	for _, w := range []int{320, 360, 375, 600, 768, 959, 1000, 1279} {
		for _, gutter := range []int{0, 15, 17} {
			layout := w - gutter
			width := valueAt(rules, "width", pane, scripted, w)
			right := valueAt(rules, "right", pane, scripted, w)
			if right != "0" && right != "0px" {
				t.Fatalf("the open pane has right %q at %dpx, want 0", right, w)
			}
			px := evalWidth(t, width, paneWidth, w, layout)
			if left := layout - px; left < 0 {
				t.Errorf("at %dpx with a %dpx scrollbar gutter the drawer is %dpx wide in a %dpx layout viewport: its left edge is at x=%d (width %q)", w, gutter, px, layout, left, width)
			}
			if w <= paneWidth+gutter && px != layout {
				t.Errorf("at %dpx (gutter %d) a narrow drawer should fill the layout viewport (%dpx), got %dpx", w, gutter, layout, px)
			}
			if w > paneWidth+gutter && px != paneWidth {
				t.Errorf("at %dpx (gutter %d) the drawer should keep its %dpx width, got %dpx", w, gutter, paneWidth, px)
			}
		}
	}
}

func loadCSS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("static", "minion.css"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// evalWidth resolves width: min(<a>, <b>) (or one length) for a fixed element: px as
// is, var(--pane-w) as the token, vw against the window width (gutter included) and
// % against the layout viewport.
func evalWidth(t *testing.T, v string, paneW, window, layout int) int {
	t.Helper()
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "min(") && strings.HasSuffix(v, ")") {
		best := -1
		for _, part := range strings.Split(v[4:len(v)-1], ",") {
			if n := evalWidth(t, part, paneW, window, layout); best < 0 || n < best {
				best = n
			}
		}
		return best
	}
	switch {
	case v == "var(--pane-w)":
		return paneW
	case strings.HasSuffix(v, "vw"):
		n, _ := strconv.Atoi(strings.TrimSuffix(v, "vw"))
		return window * n / 100
	case strings.HasSuffix(v, "%"):
		n, _ := strconv.Atoi(strings.TrimSuffix(v, "%"))
		return layout * n / 100
	case strings.HasSuffix(v, "px"):
		n, _ := strconv.Atoi(strings.TrimSuffix(v, "px"))
		return n
	}
	t.Fatalf("cannot evaluate the width %q", v)
	return 0
}

// Every editable control type takes the boundary token in both themes (the token
// itself is switched by the theme), and the controls that are not editable
// (checkbox, radio, hidden, buttons) are left to the browser.
func TestEveryEditableControlGetsTheInputBoundary(t *testing.T) {
	rules := loadSheet(t)
	editable := []string{"text", "password", "email", "search", "url", "tel", "number", "date", "time", "datetime-local", "month", "week", "made-up-type"}
	for _, ty := range editable {
		for _, w := range []int{320, 1280} {
			if b := valueAt(rules, "border", el("input", "", "type", ty), nil, w); !strings.Contains(b, "var(--input-border)") {
				t.Errorf("input type=%s has border %q at %dpx, want the --input-border boundary", ty, b, w)
			}
		}
	}
	if b := valueAt(rules, "border", el("input", ""), nil, 1280); !strings.Contains(b, "var(--input-border)") {
		t.Errorf("an input without a type (a text field) has border %q", b)
	}
	for _, tag := range []string{"select", "textarea"} {
		if b := valueAt(rules, "border", el(tag, ""), nil, 1280); !strings.Contains(b, "var(--input-border)") {
			t.Errorf("%s has border %q, want the --input-border boundary", tag, b)
		}
	}
	for _, ty := range []string{"checkbox", "radio", "hidden", "submit", "button", "file"} {
		if b := valueAt(rules, "border", el("input", "", "type", ty), nil, 1280); strings.Contains(b, "--input-border") {
			t.Errorf("input type=%s is not an editable text control but got the field border %q", ty, b)
		}
	}
	// A readonly or invalid field keeps its own look (the type list adds no specificity).
	if c := valueAt(rules, "border-color", el("input", "", "type", "text", "aria-invalid", "true"), nil, 1280); c != "var(--danger)" {
		t.Errorf("an invalid field has border-color %q, want var(--danger)", c)
	}
	if bg := valueAt(rules, "background", el("input", "", "type", "text", "readonly", ""), nil, 1280); bg != "var(--surface-subtle)" {
		t.Errorf("a readonly field has background %q, want var(--surface-subtle)", bg)
	}
	// The decorative dividers keep the quiet border token.
	if b := valueAt(rules, "border", el("div", "panel"), nil, 1280); !strings.Contains(b, "var(--border)") {
		t.Errorf("a panel has border %q, want the decorative --border", b)
	}
	// Mobile hit targets: 44px for fields from the control-height token, also for the theme select.
	if h := valueAt(rules, "--control-h", el("html", "js"), nil, 320); h != "44px" {
		t.Errorf("--control-h is %q at 320px, want 44px", h)
	}
	if h := valueAt(rules, "min-height", el("select", "", "data-theme-select", ""), []cssEl{el("label", "theme-picker")}, 320); h != "44px" {
		t.Errorf("the theme select has min-height %q at 320px, want 44px", h)
	}
}

// Every input type that the templates use is either an editable text control
// (covered by the boundary rule) or one of the controls the rule leaves alone: a
// new type in a template makes this test say which of the two it is.
func TestTemplateInputTypesAreClassified(t *testing.T) {
	left := map[string]bool{"checkbox": true, "radio": true, "hidden": true, "button": true, "submit": true, "reset": true, "image": true, "file": true, "range": true, "color": true}
	editable := map[string]bool{"text": true, "password": true, "email": true, "search": true, "url": true, "tel": true, "number": true,
		"date": true, "time": true, "datetime-local": true, "month": true, "week": true}
	files, err := filepath.Glob(filepath.Join("templates", "*.html"))
	if err != nil || len(files) == 0 {
		t.Fatalf("templates: %v %v", files, err)
	}
	inputTag := regexp.MustCompile(`<input\b[^>]*>`)
	typeAttr := regexp.MustCompile(`\btype="([^"]*)"`)
	branch := regexp.MustCompile(`\}\}([a-z-]+)\{\{`)
	rules := loadSheet(t)
	seen := map[string]int{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, tag := range inputTag.FindAllString(string(b), -1) {
			types := []string{"text"} // no type attribute: a text field
			if m := typeAttr.FindStringSubmatch(tag); m != nil {
				types = []string{m[1]}
				if strings.Contains(m[1], "{{") { // {{if .Secret}}password{{else}}text{{end}}: every branch is a type
					types = nil
					for _, br := range branch.FindAllStringSubmatch(m[1], -1) {
						types = append(types, br[1])
					}
					if len(types) == 0 {
						t.Errorf("%s: an input whose type is computed cannot be checked: %s", f, tag)
					}
				}
			}
			for _, ty := range types {
				seen[ty]++
				switch {
				case left[ty]:
				case editable[ty]:
					if b := valueAt(rules, "border", el("input", "", "type", ty), nil, 1280); !strings.Contains(b, "var(--input-border)") {
						t.Errorf("%s: input type=%s has border %q", f, ty, b)
					}
				default:
					t.Errorf("%s: input type=%q is neither an editable text control nor a known non-text control; style it or classify it here", f, ty)
				}
			}
		}
	}
	if seen["text"] == 0 || seen["password"] == 0 {
		t.Errorf("the scan found no text or password inputs (%v): the pattern is wrong", seen)
	}
}

// splitCombinators splits a complex selector into its compound selectors at the
// descendant and child combinators outside parentheses and brackets.
func splitCombinators(sel string) []string {
	var out []string
	depth, start := 0, 0
	flush := func(end int) {
		if p := strings.TrimSpace(sel[start:end]); p != "" {
			out = append(out, p)
		}
	}
	for i := 0; i < len(sel); i++ {
		switch c := sel[i]; {
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case depth == 0 && (c == ' ' || c == '>'):
			flush(i)
			start = i + 1
		}
	}
	flush(len(sel))
	return out
}
