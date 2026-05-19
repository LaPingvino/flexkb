// Package imsession is the daemon-side glue between Wayland
// keyboard_grab events and flexkb's processing pipeline. One
// Session per active text-input client: it holds the per-stack
// resolver (Physical→Substitutions baked in at compose time),
// the IM-tier FSM session (Compose chains + stateful IMs), the
// running modifier state, and the serial counter for IM commits.
//
// The daemon's hot path is:
//
//   keycode + mods   → Session.HandleKey
//                       ↓
//                       resolver.Resolve  (static layers)
//                       ↓
//                       imSession.Feed    (compose + IM tier)
//                       ↓
//                       []Action          (commit / preedit / state-change)
//
// The session translates Actions into transport-level requests
// the caller (wlim.InputMethod, or future ibus.InputContext)
// invokes. Keeping the translation out of the transport packages
// means both backends will share this orchestrator unchanged.
package imsession

import (
	"github.com/lapingvino/flexkb/internal/inputmethod"
	"github.com/lapingvino/flexkb/internal/runtime"
)

// Session carries the per-text-input mutable state. Not safe for
// concurrent calls — the daemon serialises events for one
// session on its dispatch loop (which is itself single-threaded).
type Session struct {
	resolver *runtime.Resolver
	im       *inputmethod.InputMethod
	imSess   *inputmethod.Session

	// mods is the running modifier state derived from
	// keyboard_grab's modifiers event. Updated only by
	// HandleModifiers; HandleKey reads the snapshot.
	mods runtime.ModState

	// serial monotonically increases per commit. Wayland's IM
	// protocol requires each commit() to carry a serial that
	// matches the IM's view; we don't echo a server-supplied
	// serial because v2's IM-side serial is daemon-controlled.
	serial uint32

	// keymapPending is set true after a keymap event and cleared
	// when the next done() arrives. The daemon may use it to
	// avoid emitting commits between a layout change and the
	// confirmation done — protects against committing through a
	// stale resolver.
	keymapPending bool
}

// Action is one effect the session wants the transport to perform
// in response to a keystroke. Multiple Actions per keystroke are
// possible (e.g. a stateful IM committing one candidate then
// starting a fresh preedit).
type Action interface{ isAction() }

// CommitText asks the transport to commit Text to the application.
type CommitText struct{ Text string }

// SetPreedit asks the transport to update the in-flight preedit.
// CursorBegin/End are byte offsets; -1 hides the cursor (matches
// the Wayland protocol's sentinel).
type SetPreedit struct {
	Text        string
	CursorBegin int32
	CursorEnd   int32
}

// FinishCommit asks the transport to issue its commit-the-batch
// request (e.g. zwp_input_method_v2::commit). The session has
// emitted all the queued effects for this keystroke; the
// transport flushes them as one atomic update.
type FinishCommit struct{ Serial uint32 }

// PassThrough means the session didn't claim the keystroke. The
// transport should forward it to the application unchanged.
// For Wayland this means NOT calling commit_string, NOT setting
// preedit, just letting the compositor route the key event as if
// no IME were present.
type PassThrough struct{ Symbol string }

func (CommitText) isAction()   {}
func (SetPreedit) isAction()   {}
func (FinishCommit) isAction() {}
func (PassThrough) isAction()  {}

// New creates a Session against a resolver (the static-layers
// product) and an optional InputMethod (the IM-tier rules).
// im may be nil — the session then runs in passthrough-only
// mode, useful for diagnostic builds that test the resolver
// without an IM loaded.
func New(resolver *runtime.Resolver, im *inputmethod.InputMethod) (*Session, error) {
	s := &Session{resolver: resolver, im: im}
	if im != nil {
		sess, err := inputmethod.NewSession(im)
		if err != nil {
			return nil, err
		}
		s.imSess = sess
	}
	return s, nil
}

// HandleKey processes one Linux evdev scancode + the current
// modifier state. state values match Wayland: 0=released,
// 1=pressed. Key releases produce no actions in v1 (the IM
// tier is press-driven); they may carry state for future
// "compose-cancel-on-release" features.
func (s *Session) HandleKey(scancode uint32, state uint32) []Action {
	if state == 0 {
		return nil
	}
	keyName, ok := runtime.XKBNameForLinuxScancode(scancode)
	if !ok {
		// Unmapped scancode (function keys, media, modifiers
		// themselves): nothing to feed the engine. Transport
		// decides whether to forward to the app; we report
		// passthrough so it has the option.
		return []Action{PassThrough{}}
	}
	sym, ok := s.resolver.Resolve(keyName, s.mods)
	if !ok {
		return []Action{PassThrough{}}
	}
	if s.imSess == nil {
		// Resolver-only mode: emit the symbol as a commit so
		// the daemon still passes characters through. Useful
		// for diagnostic builds where you want to verify the
		// static-layer side without an IM loaded.
		return s.singleCommit(sym)
	}
	events := s.imSess.Feed(sym)
	return s.translate(events, sym)
}

// HandleModifiers updates the session's view of modifier state.
// Wayland delivers depressed/latched/locked bitmasks per its
// xkb-derived model; we translate to the resolver's ModState
// using xkb's well-known indices.
//
// xkb modifier index assignments (from xkb's default keymap):
//
//	0 = Shift
//	1 = Lock        (caps lock)
//	2 = Control
//	3 = Mod1        (typically Alt)
//	4 = Mod2        (typically Num Lock)
//	5 = Mod3        (rare)
//	6 = Mod4        (typically Super/Windows)
//	7 = Mod5        (typically AltGr / ISO_Level3_Shift)
func (s *Session) HandleModifiers(depressed, latched, locked uint32) {
	mods := depressed | latched | locked
	s.mods.Shift = (mods>>0)&1 == 1
	s.mods.CapsLock = (mods>>1)&1 == 1
	s.mods.AltGr = (mods>>7)&1 == 1
}

// MarkKeymapPending is called when keyboard_grab delivers a new
// keymap event. The daemon should not emit commits until the
// next done() arrives — between those, the resolver may be
// stale (it was built against an earlier keymap).
func (s *Session) MarkKeymapPending() { s.keymapPending = true }

// MarkKeymapApplied is called when the next done() event closes
// the keymap-change batch. The daemon may resume commits.
func (s *Session) MarkKeymapApplied() { s.keymapPending = false }

// translate converts IM-tier Events into transport-level Actions.
// Coalesces preedit + commit into a single FinishCommit at the
// end so the transport issues one atomic commit() per keystroke.
func (s *Session) translate(events []inputmethod.Event, originalSym string) []Action {
	if s.keymapPending {
		// Suppress mid-keymap commits. The keystroke that
		// produced events was processed (the IM's internal
		// state advanced); we just don't tell the transport
		// to commit it.
		return nil
	}
	if len(events) == 0 {
		// IM swallowed the symbol entirely (buffered, no
		// visible effect yet). Don't emit FinishCommit — the
		// transport has nothing to flush.
		return nil
	}
	var out []Action
	committed := false
	for _, ev := range events {
		switch e := ev.(type) {
		case inputmethod.CommitEvent:
			out = append(out, CommitText{Text: e.Text})
			committed = true
		case inputmethod.PreeditEvent:
			out = append(out, SetPreedit{Text: e.Text, CursorBegin: int32(len(e.Text)), CursorEnd: int32(len(e.Text))})
			committed = true
		case inputmethod.PassthroughEvent:
			// IM passed the symbol through. Use the symbol the
			// resolver produced (which is what the app expects
			// the keypress to mean) rather than e.Symbol
			// (which would be the same thing here, but might
			// differ once we layer in dead-key handling).
			_ = e
			out = append(out, PassThrough{Symbol: originalSym})
		case inputmethod.StateChangeEvent:
			// Internal-only — useful for debugging but the
			// transport never sees it.
		}
	}
	if committed {
		s.serial++
		out = append(out, FinishCommit{Serial: s.serial})
	}
	return out
}

// singleCommit is the no-IM-loaded shortcut: produce one
// CommitText + FinishCommit pair for a single symbol. Used when
// Session was constructed with a nil im so the resolver-only
// pipeline still delivers characters to applications.
func (s *Session) singleCommit(sym string) []Action {
	s.serial++
	return []Action{
		CommitText{Text: sym},
		FinishCommit{Serial: s.serial},
	}
}
