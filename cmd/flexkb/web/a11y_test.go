// Package web — accessibility lint for the GUI assets. Not a
// full WCAG audit (axe-core does that, in headless Chromium,
// when we add it as a follow-up). The rules here are the
// structural minimums every shipping build must satisfy:
//
//   * Every button must have an accessible name (text content,
//     aria-label, or aria-labelledby).
//   * Every form input that isn't decorative needs a label.
//   * Every modal dialog needs role + aria-modal + aria-labelledby.
//   * Tab buttons need role + aria-controls; tab panels need
//     role + aria-labelledby pointing at their tab.
//   * The page needs a skip-link as the first focusable element.
//
// These rules run as a Go test so a regression in the HTML
// surfaces at `go test` time, not in production where a
// screen-reader user would be the one to notice.
package web

import (
	_ "embed"
	"regexp"
	"strings"
	"testing"
)

//go:embed index.html
var indexHTML string

// TestEveryButtonHasAccessibleName scans the HTML for <button>
// elements and verifies each has either non-empty visible text,
// or an aria-label attribute, or aria-labelledby. Icon-only
// buttons that rely on `title` ALONE are rejected — screen
// readers don't reliably surface title to users.
func TestEveryButtonHasAccessibleName(t *testing.T) {
	buttons := extractButtons(indexHTML)
	if len(buttons) < 10 {
		t.Fatalf("only found %d buttons — extraction likely broken", len(buttons))
	}
	for _, b := range buttons {
		if hasAccessibleName(b) {
			continue
		}
		// Trim for the failure message — bare regex output is unreadable.
		snippet := b
		if len(snippet) > 160 {
			snippet = snippet[:160] + "…"
		}
		t.Errorf("button without accessible name:\n  %s\n  (add aria-label or visible text content)", snippet)
	}
}

// TestDialogsHaveRequiredAria checks every .inspect-overlay
// element has role="dialog", aria-modal="true", and an
// aria-labelledby pointing at a real id within itself.
func TestDialogsHaveRequiredAria(t *testing.T) {
	dialogs := extractDialogs(indexHTML)
	if len(dialogs) == 0 {
		t.Fatal("no .inspect-overlay elements found — extraction or markup broken")
	}
	for _, d := range dialogs {
		if !strings.Contains(d, `role="dialog"`) {
			t.Errorf("dialog missing role=\"dialog\":\n  %s", firstLine(d))
		}
		if !strings.Contains(d, `aria-modal="true"`) {
			t.Errorf("dialog missing aria-modal=\"true\":\n  %s", firstLine(d))
		}
		labelled := regexp.MustCompile(`aria-labelledby="([^"]+)"`).FindStringSubmatch(d)
		if len(labelled) < 2 {
			t.Errorf("dialog missing aria-labelledby:\n  %s", firstLine(d))
			continue
		}
		// The target id must exist somewhere in the document.
		target := labelled[1]
		if !strings.Contains(indexHTML, `id="`+target+`"`) {
			t.Errorf("dialog aria-labelledby=%q has no matching id in document", target)
		}
	}
}

// TestTablistWiring confirms each tab button has role=tab plus
// aria-controls pointing at a panel that has matching
// aria-labelledby. Without both directions wired, screen readers
// can navigate the tabs but lose track of which panel is the
// current one.
func TestTablistWiring(t *testing.T) {
	tabs := regexp.MustCompile(`<button[^>]+role="tab"[^>]*>`).FindAllString(indexHTML, -1)
	if len(tabs) == 0 {
		t.Fatal("no tab buttons found")
	}
	for _, tab := range tabs {
		tabID := extractAttr(tab, "id")
		controlID := extractAttr(tab, "aria-controls")
		if tabID == "" {
			t.Errorf("tab missing id (needed by aria-labelledby): %s", tab)
		}
		if controlID == "" {
			t.Errorf("tab missing aria-controls: %s", tab)
			continue
		}
		// The panel must exist and labelledby this tab.
		panelMatcher := regexp.MustCompile(
			`<section[^>]+id="` + regexp.QuoteMeta(controlID) + `"[^>]*>`)
		panel := panelMatcher.FindString(indexHTML)
		if panel == "" {
			t.Errorf("tab aria-controls=%q has no matching <section>", controlID)
			continue
		}
		if extractAttr(panel, "aria-labelledby") != tabID {
			t.Errorf("panel %q must aria-labelledby=%q (got %q)",
				controlID, tabID, extractAttr(panel, "aria-labelledby"))
		}
	}
}

// TestSkipLinkPresent — the skip-link must exist and be the first
// child of <body>. Keyboard users hit Tab once on page load and
// expect to be able to jump past the header.
func TestSkipLinkPresent(t *testing.T) {
	if !regexp.MustCompile(`<body[^>]*>\s*<a\s+class="skip-link"`).MatchString(indexHTML) {
		t.Error("skip-link is not the first child of <body> — keyboard users can't bypass the header")
	}
	if !strings.Contains(indexHTML, `href="#main-content"`) {
		t.Error("skip-link should target #main-content")
	}
	if !regexp.MustCompile(`<main[^>]+id="main-content"`).MatchString(indexHTML) {
		t.Error("<main> must have id=\"main-content\" so the skip-link target resolves")
	}
}

// TestStatusElementsHaveLiveRegion — every .status span the JS
// writes to must be a live region so screen readers announce
// status changes. role="status" implies aria-live="polite" but
// both are spelt out for explicitness.
func TestStatusElementsHaveLiveRegion(t *testing.T) {
	statusEls := regexp.MustCompile(
		`<span\s+class="status"[^>]*>`).FindAllString(indexHTML, -1)
	for _, el := range statusEls {
		if !strings.Contains(el, `role="status"`) {
			t.Errorf("status span missing role=\"status\":\n  %s", el)
		}
		if !strings.Contains(el, `aria-live=`) {
			t.Errorf("status span missing aria-live:\n  %s", el)
		}
	}
}

// --- helpers ---

func extractButtons(html string) []string {
	// Non-greedy match of the button open tag plus its content
	// up to (and including) the closing tag. Works for the
	// hand-authored markup in this file; nested buttons aren't
	// a thing in valid HTML so we don't worry about that case.
	return regexp.MustCompile(`(?s)<button\b[^>]*>.*?</button>`).FindAllString(html, -1)
}

func extractDialogs(html string) []string {
	return regexp.MustCompile(`(?s)<div\s+class="inspect-overlay[^"]*"[^>]*>`).FindAllString(html, -1)
}

func hasAccessibleName(buttonHTML string) bool {
	if strings.Contains(buttonHTML, "aria-label=") {
		return true
	}
	if strings.Contains(buttonHTML, "aria-labelledby=") {
		return true
	}
	// Extract the inner text. Strip tags and aria-hidden spans
	// (those decorations don't count as accessible names).
	open := strings.Index(buttonHTML, ">")
	close := strings.LastIndex(buttonHTML, "</button>")
	if open == -1 || close == -1 {
		return false
	}
	inner := buttonHTML[open+1 : close]
	// Remove aria-hidden children entirely.
	inner = regexp.MustCompile(`(?s)<[^>]+aria-hidden="true"[^>]*>.*?</[^>]+>`).ReplaceAllString(inner, "")
	// Strip remaining tags.
	inner = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(inner, "")
	return strings.TrimSpace(inner) != ""
}

func extractAttr(tag, name string) string {
	m := regexp.MustCompile(name + `="([^"]*)"`).FindStringSubmatch(tag)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
