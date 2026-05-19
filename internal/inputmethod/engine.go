package inputmethod

import (
	"fmt"
	"strings"
)

// Event is what an IM session emits in response to feeding a symbol.
// Consumers (the Wayland daemon, tests, the GUI preview) interpret
// these to drive the application/UI.
type Event interface{ isIMEvent() }

// PreeditEvent updates the user-visible "in-flight" composition. The
// application shows Text under the cursor without committing it.
type PreeditEvent struct{ Text string }

// CommitEvent finalises Text into the application's text buffer.
// Multiple CommitEvents in a single Feed call are valid (e.g. a
// transition that both commits a candidate and starts a fresh
// preedit).
type CommitEvent struct{ Text string }

// StateChangeEvent records FSM state transitions. The engine emits
// these for observability/debugging; consumers typically ignore them.
type StateChangeEvent struct{ From, To string }

// PassthroughEvent fires when no transition matched and the IM lets
// the symbol fall through to the application unchanged. Consumers
// translate this back into a regular key event.
type PassthroughEvent struct{ Symbol string }

func (PreeditEvent) isIMEvent()     {}
func (CommitEvent) isIMEvent()      {}
func (StateChangeEvent) isIMEvent() {}
func (PassthroughEvent) isIMEvent() {}

// Session is one user's in-flight IM state for a single text-input
// connection. Reuse across Feed calls; one session per Wayland
// text_input client.
type Session struct {
	im     *InputMethod
	state  string
	buffer []string
	// lastLookup is the most recent dictionary result, exposed to
	// transition templates via ${lookup.top} / ${lookup.candidate[N]}.
	// Cleared on commit/clear-buffer.
	lastLookup []string
	// classes is the resolved match-class table (builtins overlaid
	// by the IM's custom classes). Computed once at session start.
	classes map[string]map[string]bool
}

// NewSession creates a Session anchored at the IM's initial state.
// Returns an error if the IM has no states or no initial state.
func NewSession(im *InputMethod) (*Session, error) {
	if im == nil {
		return nil, fmt.Errorf("nil InputMethod")
	}
	if len(im.States) == 0 {
		return nil, fmt.Errorf("InputMethod %q has no states", im.Name)
	}
	s := &Session{
		im:      im,
		state:   im.States[0].ID,
		classes: buildClasses(im),
	}
	return s, nil
}

// State returns the session's current FSM state ID. Exposed for
// debugging and for the GUI's "what's the IM thinking right now"
// panel.
func (s *Session) State() string { return s.state }

// Buffer returns a copy of the session's current buffer contents.
func (s *Session) Buffer() []string {
	out := make([]string, len(s.buffer))
	copy(out, s.buffer)
	return out
}

// Feed processes one input symbol through the IM. Returns the
// resulting event sequence in order. An empty event slice means the
// symbol was absorbed by the IM (buffered) without externally-visible
// effect.
//
// Symbols are passed as their xkb-keysym names ("a", "BackSpace",
// "Multi_key") OR as single-character literals for printable ASCII.
// The engine normalises single-char literals before matching.
func (s *Session) Feed(sym string) []Event {
	st, ok := s.findState(s.state)
	if !ok {
		// Shouldn't happen if NewSession succeeded; defensive.
		return []Event{PassthroughEvent{Symbol: sym}}
	}
	for _, tr := range st.Transitions {
		if !s.matches(tr, sym) {
			continue
		}
		return s.apply(tr, sym)
	}
	// No transition matched — the IM doesn't claim this symbol.
	return []Event{PassthroughEvent{Symbol: sym}}
}

// findState locates a state by ID. Returns ok=false if absent.
func (s *Session) findState(id string) (State, bool) {
	for _, st := range s.im.States {
		if st.ID == id {
			return st, true
		}
	}
	return State{}, false
}

// matches returns true iff the transition fires on this symbol.
// Match (literal) and MatchClass are exclusive; if both are set,
// Match wins (and MatchClass is ignored).
func (s *Session) matches(tr Transition, sym string) bool {
	if tr.Match != "" {
		if tr.Match == "*" {
			return true
		}
		return tr.Match == sym
	}
	if tr.MatchClass != "" {
		set, ok := s.classes[tr.MatchClass]
		if !ok {
			return false
		}
		return set[sym]
	}
	// Empty transition matches nothing (use Match: "*" for wildcards).
	return false
}

// apply runs the transition's effects in canonical order and
// produces the event stream they generated. Effect order matches
// author intent: a transition that wants to commit-then-clear writes
// `commit: "${buffer}", clear_buffer: true` and expects the commit to
// see the pre-clear buffer. So:
//
//  1. Additive buffer ops (append/pop) — they modify what Emit sees.
//  2. Lookup — populated from the post-additive buffer state.
//  3. Emit (preedit and/or commit) — template expansion uses the
//     buffer state visible after additive ops + lookup result.
//  4. Destructive buffer ops (clear) — happen AFTER emit so the
//     commit can capture the buffer before it's wiped.
//  5. State change (Goto / GotoIfEmpty).
//  6. OnEnter of the destination state.
func (s *Session) apply(tr Transition, sym string) []Event {
	var events []Event

	// 1. Additive buffer ops
	if tr.AppendBuffer {
		s.buffer = append(s.buffer, sym)
	}
	if tr.PopBuffer && len(s.buffer) > 0 {
		s.buffer = s.buffer[:len(s.buffer)-1]
	}

	// 2. Lookup
	if tr.Lookup != nil {
		s.lastLookup = s.runLookup(tr.Lookup)
	}

	// 3. Emit (preedit and/or commit) — captures pre-clear buffer.
	if tr.Action.Commit != "" {
		text := s.expand(tr.Action.Commit, sym)
		events = append(events, CommitEvent{Text: text})
	}
	if tr.Action.Preedit != "" {
		text := s.expand(tr.Action.Preedit, sym)
		events = append(events, PreeditEvent{Text: text})
	}

	// 4. Destructive buffer ops — after emit.
	if tr.ClearBuffer {
		s.buffer = s.buffer[:0]
		s.lastLookup = nil
	}

	// State navigation. GotoIfEmpty wins over Goto when its
	// condition is met (lets a single transition both pop and exit).
	next := s.state
	if tr.GotoIfEmpty != "" && len(s.buffer) == 0 {
		next = tr.GotoIfEmpty
	} else if tr.Goto != "" {
		next = tr.Goto
	}
	if next != s.state {
		events = append(events, StateChangeEvent{From: s.state, To: next})
		s.state = next
		if st, ok := s.findState(next); ok && (st.OnEnter.Preedit != "" || st.OnEnter.Commit != "") {
			if st.OnEnter.Commit != "" {
				events = append(events, CommitEvent{Text: s.expand(st.OnEnter.Commit, sym)})
			}
			if st.OnEnter.Preedit != "" {
				events = append(events, PreeditEvent{Text: s.expand(st.OnEnter.Preedit, sym)})
			}
		}
	}
	return events
}

// runLookup is a placeholder for the dictionary subsystem. Real
// implementation will resolve tr.Lookup.Dictionary to a loaded
// dictionary file and rank candidates per tr.Lookup.Rank.
// For now it returns the buffer itself as a one-element candidate
// list so templates that reference ${lookup.top} still work for
// engine-shape tests.
func (s *Session) runLookup(_ *Lookup) []string {
	return []string{strings.Join(s.buffer, "")}
}

// expand resolves template variables in a Preedit/Commit string.
// Supported variables (no others — fail-soft: unknown vars left as-is
// so authors notice and report rather than silently misbehaving):
//
//	${buffer}                — buffer contents concatenated
//	${match}                 — the symbol that triggered this transition
//	${lookup.top}            — first candidate from the last Lookup
//	${lookup.candidate[N]}   — Nth candidate (1-indexed)
func (s *Session) expand(tmpl, match string) string {
	out := tmpl
	out = strings.ReplaceAll(out, "${buffer}", strings.Join(s.buffer, ""))
	out = strings.ReplaceAll(out, "${match}", match)
	if len(s.lastLookup) > 0 {
		out = strings.ReplaceAll(out, "${lookup.top}", s.lastLookup[0])
		for i, c := range s.lastLookup {
			needle := fmt.Sprintf("${lookup.candidate[%d]}", i+1)
			out = strings.ReplaceAll(out, needle, c)
		}
	}
	return out
}

// buildClasses overlays the IM's custom match_classes onto the
// engine builtins. Custom entries shadow builtins of the same name,
// letting an IM tighten or redefine a class if needed.
func buildClasses(im *InputMethod) map[string]map[string]bool {
	out := make(map[string]map[string]bool)
	for name, members := range builtinClasses {
		set := make(map[string]bool, len(members))
		for _, m := range members {
			set[m] = true
		}
		out[name] = set
	}
	for name, members := range im.MatchClasses {
		set := make(map[string]bool, len(members))
		for _, m := range members {
			set[m] = true
		}
		out[name] = set
	}
	return out
}

// builtinClasses ships equivalence sets every IM can reference.
// Custom classes in the IM file can override these by name.
var builtinClasses = map[string][]string{
	"latin-letter":  splitChars("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"),
	"latin-lower":   splitChars("abcdefghijklmnopqrstuvwxyz"),
	"latin-upper":   splitChars("ABCDEFGHIJKLMNOPQRSTUVWXYZ"),
	"latin-vowel":   splitChars("aeiouAEIOU"),
	"pinyin-letter": splitChars("abcdefghijklmnopqrstuvwxyz"),
	"digit":         splitChars("0123456789"),
	"whitespace":    {"space", "Tab"},
	"editing":       {"BackSpace", "Delete", "Left", "Right", "Home", "End"},
}

func splitChars(s string) []string {
	out := make([]string, 0, len(s))
	for _, r := range s {
		out = append(out, string(r))
	}
	return out
}

// EffectiveBackspace resolves the backspace-mode default per the
// rules in docs/DESIGN-IME.md "Per-IM defaults". When the IM file
// sets Backspace explicitly that value wins; otherwise the engine
// infers based on IM shape: dictionary-using IMs default to
// "reopen", multi-state IMs to "decompose", everything else to
// "passthrough".
func (im *InputMethod) EffectiveBackspace() string {
	if im.Backspace != "" {
		return im.Backspace
	}
	if len(im.Dictionaries) > 0 {
		return "reopen"
	}
	if len(im.States) > 1 {
		return "decompose"
	}
	return "passthrough"
}

// EffectiveLearning resolves the learning-mode default. Explicit
// value wins; otherwise enabled iff the IM declares any dictionaries.
func (im *InputMethod) EffectiveLearning() string {
	if im.Learning != "" {
		return im.Learning
	}
	if len(im.Dictionaries) > 0 {
		return "enabled"
	}
	return "disabled"
}
