package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestLiquidGlassMaterial loads a real page and checks the stylesheet the
// handler actually shipped. Chrome (sidebar, tab bar, sheets) uses the
// translucent blurred material and a specular edge. Buttons, dialogs, and
// reading surfaces stay opaque so labels do not show through. Dark
// restyles the material, reduced
// transparency frosts it, and reduced motion drops decorative animation.
func TestLiquidGlassMaterial(t *testing.T) {
	srv, _, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="page-home"`) {
		t.Fatal("home page marker missing; stylesheet was not read from a rendered page")
	}
	if !strings.Contains(body, `hx-target="#live-root"`) {
		t.Fatal("boosted shell target missing")
	}
	css := stripCSSComments(extractStyles(t, body))
	blocks := parseCSSBlocks(css)

	light, ok := firstPrelude(blocks, ":root")
	if !ok {
		t.Fatal("light :root missing")
	}
	lightTint := mustAlpha(t, "light --glass-tint", mustProp(t, light, "--glass-tint"))
	lightHighlight := mustProp(t, light, "--glass-highlight")
	if !strings.Contains(lightHighlight, "inset") {
		t.Fatalf("specular highlight must be an inset edge, got %q", lightHighlight)
	}
	if !strings.Contains(mustProp(t, light, "--glass-material"), "blur(") {
		t.Fatal("--glass-material does not blur")
	}
	if lightTint >= 100 {
		t.Fatalf("glass tint is opaque: %v", lightTint)
	}
	if _, translucent := slashAlpha(mustProp(t, light, "--button")); translucent {
		t.Fatal("primary button fill must be opaque")
	}

	darkBody, ok := mediaRoot(blocks, func(pre string) bool {
		return strings.Contains(pre, "prefers-color-scheme: dark") && !strings.Contains(pre, "prefers-reduced-transparency")
	})
	if !ok {
		t.Fatal("dark :root missing")
	}
	darkTint := mustAlpha(t, "dark --glass-tint", mustProp(t, darkBody, "--glass-tint"))
	darkHighlight := mustProp(t, darkBody, "--glass-highlight")
	if darkTint == lightTint || darkHighlight == lightHighlight {
		t.Fatalf("dark scheme did not restyle the material (%v %q)", darkTint, darkHighlight)
	}
	if _, translucent := slashAlpha(mustProp(t, darkBody, "--button")); translucent {
		t.Fatal("dark primary button fill must be opaque")
	}
	if !strings.Contains(darkHighlight, "inset") {
		t.Fatalf("dark highlight lost its specular inset: %q", darkHighlight)
	}

	frostBody, ok := mediaRoot(blocks, func(pre string) bool {
		return strings.Contains(pre, "prefers-reduced-transparency") && !strings.Contains(pre, "prefers-color-scheme")
	})
	if !ok {
		t.Fatal("reduced-transparency :root missing")
	}
	frostTint := mustAlpha(t, "frost --glass-tint", mustProp(t, frostBody, "--glass-tint"))
	if frostTint <= lightTint {
		t.Fatalf("reduced transparency must frost the tint: light %v frost %v", lightTint, frostTint)
	}
	darkFrostBody, ok := mediaRoot(blocks, func(pre string) bool {
		return strings.Contains(pre, "prefers-color-scheme: dark") && strings.Contains(pre, "prefers-reduced-transparency")
	})
	if !ok {
		t.Fatal("dark reduced-transparency :root missing")
	}
	darkFrost := mustAlpha(t, "dark frost --glass-tint", mustProp(t, darkFrostBody, "--glass-tint"))
	if darkFrost <= darkTint {
		t.Fatalf("dark reduced transparency must frost the tint: dark %v frost %v", darkTint, darkFrost)
	}

	if !mediaHas(blocks, "prefers-reduced-motion", "animation: none") {
		t.Fatal("reduced motion does not disable decorative animation")
	}

	for _, sel := range []string{"aside.side::before", ".tabbar"} {
		rule, found := ruleWithSelector(blocks, sel)
		if !found {
			t.Fatalf("missing chrome rule %s", sel)
		}
		if !hasGlassMaterial(rule) {
			t.Fatalf("%s is not a translucent blurred material with a specular highlight", sel)
		}
	}
	// The phone action sheet is a reading surface: its cards drop their own
	// fill, so a glass tint leaves the page readable through them. Same rule
	// as dialogs. The desktop rail is only a column and has no fill of its own.
	var sheetOpaque, headOpaque bool
	for _, rule := range rulesWithSelector(blocks, ".pr-rail") {
		if strings.Contains(rule, "backdrop-filter") {
			t.Fatal("action sheet must not blur; the page shows through the cards")
		}
		if strings.Contains(rule, "background: var(--surface)") {
			sheetOpaque = true
		}
	}
	if !sheetOpaque {
		t.Fatal("phone action sheet must be an opaque surface")
	}
	for _, rule := range rulesWithSelector(blocks, ".rail-sheet-head") {
		if strings.Contains(rule, "backdrop-filter") {
			t.Fatal("action sheet header must not blur")
		}
		if strings.Contains(rule, "background: var(--surface)") {
			headOpaque = true
		}
	}
	if !headOpaque {
		t.Fatal("action sheet header must be an opaque surface")
	}
	tabbar, found := ruleWithSelector(blocks, ".tabbar")
	if !found || !strings.Contains(tabbar, "border-radius: var(--glass-radius)") || !strings.Contains(tabbar, "var(--side-inset)") {
		t.Fatal("phone tab bar is not a floating glass footer")
	}
	menu, found := ruleWithSelector(blocks, ".proj-menu")
	if !found || !strings.Contains(menu, "background: var(--surface)") || strings.Contains(menu, "backdrop-filter") {
		t.Fatal("project menu must be an opaque surface so the nav does not show through")
	}
	for _, sel := range []string{".gw-confirm-modal", ".gw-stream-modal"} {
		rule, ok := ruleWithSelector(blocks, sel)
		if !ok || !strings.Contains(rule, "background: var(--surface)") || strings.Contains(rule, "backdrop-filter") {
			t.Fatalf("%s must be an opaque surface so the page does not show through", sel)
		}
	}
	btn, found := ruleWithSelector(blocks, "button")
	if !found || strings.Contains(btn, "backdrop-filter") || !strings.Contains(btn, "background: var(--button)") {
		t.Fatal("primary buttons must be an opaque fill, not glass")
	}
	secondary, found := ruleWithSelector(blocks, ".btn-secondary")
	if !found || !strings.Contains(secondary, "background: var(--surface)") || strings.Contains(secondary, "var(--glass-tint)") {
		t.Fatal("secondary buttons must be an opaque surface")
	}

	for _, sel := range []string{".card", ".m-rows", ".dpatch", ".gw-stream-log", ".table-scroll", ".fcard", ".review-bar", ".sr-rows", ".composer"} {
		rules := rulesWithSelector(blocks, sel)
		if len(rules) == 0 {
			t.Fatalf("missing content rule %s", sel)
		}
		for _, rule := range rules {
			if strings.Contains(rule, "backdrop-filter") {
				t.Fatalf("content surface %s uses backdrop-filter", sel)
			}
		}
	}

	asideRules := rulesWithSelector(blocks, "aside.side")
	if len(asideRules) == 0 {
		t.Fatal("aside.side rule missing")
	}
	for _, rule := range asideRules {
		if strings.Contains(rule, "backdrop-filter") || strings.Contains(rule, "transform:") {
			t.Fatal("aside.side must not carry backdrop-filter or transform; that traps the phone tab bar")
		}
	}
	// The panel is position:fixed, so it does not reserve a column. The
	// content padding has to start past inset + width, or titles sit under
	// the glass. Phone resets this with its own shorthand.
	var clearsSidebar bool
	for _, rule := range rulesWithSelector(blocks, "main.main") {
		if strings.Contains(rule, "calc(var(--side-inset) + var(--side-width) + var(--side-gap))") {
			clearsSidebar = true
		}
	}
	if !clearsSidebar {
		t.Fatal("desktop content column does not clear the floating sidebar")
	}
	sideRule, found := ruleWithSelector(blocks, "aside.side")
	if !found || !strings.Contains(sideRule, "width: var(--side-width)") || !strings.Contains(sideRule, "left: var(--side-inset)") {
		t.Fatal("sidebar width and inset must be the same metrics the content column clears")
	}
	for _, rule := range rulesMatching(blocks, func(sel string) bool { return strings.Contains(sel, "#live-root") }) {
		if strings.Contains(rule, "backdrop-filter") || strings.Contains(rule, "transform:") || hasFilterProp(rule) {
			t.Fatal("#live-root must not carry filter, backdrop-filter, or transform")
		}
	}
	if strings.Contains(body, `backdrop-filter: blur(10px)`) {
		t.Fatal("review bar blur is back; iOS samples it into the status bar")
	}
}

func extractStyles(t *testing.T, body string) string {
	t.Helper()
	var b strings.Builder
	rest := body
	for {
		i := strings.Index(rest, "<style>")
		if i < 0 {
			break
		}
		rest = rest[i+len("<style>"):]
		j := strings.Index(rest, "</style>")
		if j < 0 {
			t.Fatal("unclosed style")
		}
		b.WriteString(rest[:j])
		b.WriteByte('\n')
		rest = rest[j+len("</style>"):]
	}
	if b.Len() == 0 {
		t.Fatal("page shipped no stylesheet")
	}
	return b.String()
}

func stripCSSComments(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		j := strings.Index(s[i+2:], "*/")
		if j < 0 {
			return b.String()
		}
		s = s[i+2+j+2:]
	}
}

type cssBlock struct {
	prelude string
	body    string
	nested  []cssBlock
}

func parseCSSBlocks(css string) []cssBlock {
	var out []cssBlock
	i := 0
	for i < len(css) {
		for i < len(css) && isCSSSpace(css[i]) {
			i++
		}
		if i >= len(css) {
			break
		}
		rel := strings.IndexByte(css[i:], '{')
		if rel < 0 {
			break
		}
		prelude := strings.TrimSpace(css[i : i+rel])
		i += rel + 1
		depth := 1
		start := i
		for i < len(css) && depth > 0 {
			switch css[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth > 0 {
				i++
			}
		}
		if i >= len(css) {
			break
		}
		body := css[start:i]
		i++ // closing brace
		block := cssBlock{prelude: prelude, body: body}
		if strings.HasPrefix(prelude, "@") && prelude != "@font-face" {
			block.nested = parseCSSBlocks(body)
		}
		out = append(out, block)
	}
	return out
}

func firstPrelude(blocks []cssBlock, want string) (string, bool) {
	for _, b := range blocks {
		if strings.TrimSpace(b.prelude) == want {
			return b.body, true
		}
	}
	return "", false
}

func mediaRoot(blocks []cssBlock, pred func(string) bool) (string, bool) {
	var found string
	var ok bool
	var walk func([]cssBlock)
	walk = func(bs []cssBlock) {
		for _, b := range bs {
			if strings.HasPrefix(b.prelude, "@media") && pred(b.prelude) {
				for _, inner := range b.nested {
					if strings.TrimSpace(inner.prelude) == ":root" {
						found = inner.body
						ok = true
					}
				}
			}
			walk(b.nested)
		}
	}
	walk(blocks)
	return found, ok
}

func mediaHas(blocks []cssBlock, preludePart, bodyPart string) bool {
	var ok bool
	var walk func([]cssBlock)
	walk = func(bs []cssBlock) {
		for _, b := range bs {
			if strings.HasPrefix(b.prelude, "@media") && strings.Contains(b.prelude, preludePart) && strings.Contains(b.body, bodyPart) {
				ok = true
			}
			walk(b.nested)
		}
	}
	walk(blocks)
	return ok
}

func ruleWithSelector(blocks []cssBlock, sel string) (string, bool) {
	rules := rulesWithSelector(blocks, sel)
	for _, rule := range rules {
		if hasGlassMaterial(rule) {
			return rule, true
		}
	}
	if len(rules) == 0 {
		return "", false
	}
	return rules[0], true
}

func rulesWithSelector(blocks []cssBlock, sel string) []string {
	return rulesMatching(blocks, func(item string) bool { return item == sel })
}

func rulesMatching(blocks []cssBlock, pred func(string) bool) []string {
	var out []string
	var walk func([]cssBlock)
	walk = func(bs []cssBlock) {
		for _, b := range bs {
			if strings.HasPrefix(strings.TrimSpace(b.prelude), "@") {
				walk(b.nested)
				continue
			}
			for item := range strings.SplitSeq(b.prelude, ",") {
				if pred(strings.TrimSpace(item)) {
					out = append(out, b.body)
					break
				}
			}
			walk(b.nested)
		}
	}
	walk(blocks)
	return out
}

func hasGlassMaterial(body string) bool {
	if !strings.Contains(body, "backdrop-filter: var(--glass-material)") {
		return false
	}
	if !strings.Contains(body, "var(--glass-highlight)") {
		return false
	}
	return strings.Contains(body, "var(--glass-tint)") || strings.Contains(body, "var(--button)")
}

func hasFilterProp(body string) bool {
	rest := body
	for {
		i := strings.Index(rest, "filter:")
		if i < 0 {
			return false
		}
		if i >= len("backdrop-") && rest[i-len("backdrop-"):i] == "backdrop-" {
			rest = rest[i+len("filter:"):]
			continue
		}
		return true
	}
}

func mustProp(t *testing.T, body, name string) string {
	t.Helper()
	v, ok := customProp(body, name)
	if !ok {
		t.Fatalf("missing %s", name)
	}
	return v
}

func customProp(body, name string) (string, bool) {
	rest := body
	for {
		i := strings.Index(rest, name)
		if i < 0 {
			return "", false
		}
		if (i == 0 || !isCSSIdent(rest[i-1])) && i+len(name) < len(rest) && rest[i+len(name)] == ':' {
			v := strings.TrimSpace(rest[i+len(name)+1:])
			end, _, _ := strings.Cut(v, ";")
			return strings.TrimSpace(end), true
		}
		rest = rest[i+len(name):]
	}
}

func mustAlpha(t *testing.T, label, value string) float64 {
	t.Helper()
	a, ok := slashAlpha(value)
	if !ok {
		t.Fatalf("%s has no alpha: %q", label, value)
	}
	return a
}

func slashAlpha(value string) (float64, bool) {
	i := strings.LastIndex(value, "/")
	if i < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(value[i+1:])
	rest = strings.TrimRight(rest, "); \t")
	rest, ok := strings.CutSuffix(rest, "%")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func isCSSSpace(b byte) bool {
	return b == ' ' || b == '\n' || b == '\t' || b == '\r'
}

func isCSSIdent(b byte) bool {
	return b == '-' || b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
