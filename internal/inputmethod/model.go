// Package inputmethod implements the stateful sixth tier of flexkb's
// modular layer stack. See docs/DESIGN-IME.md for the architectural
// contract; this package is the engine: schema types, YAML loader,
// FSM evaluator. The Wayland daemon, GUI integration, and m17n-mim
// import live in separate packages built on top of this one.
//
// An InputMethod consumes a stream of input symbols (the output of
// every layer below — Physical/Transformation/Additions/Substitutions/
// Compose) and emits a stream of Events (preedit updates, commits,
// state changes) back to the consumer. The IM is the only flexkb
// layer that carries unbounded state across events.
package inputmethod

// InputMethod is one input-method rule file (data/inputmethods/<name>.yaml).
// All fields except Name/States are optional with sensible defaults.
type InputMethod struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`

	// InputLayer declares which symbol stream this IM expects. The
	// validator uses it to reject incoherent stacks (e.g. Pinyin IM
	// on top of a Cyrillic-output transformation). Conventional
	// values: "latin", "cyrillic", "greek", "hebrew", "arabic",
	// "devanagari", … or "any" to accept whatever lower layers
	// produce.
	InputLayer string `yaml:"input_layer,omitempty"`

	// OutputScript is the ISO 15924 four-letter tag of what this IM
	// commits. Used by compositional validation to check whether
	// stacking this IM provides the characters a layout's locale
	// requires. Examples: "hani" (Han), "deva" (Devanagari),
	// "kore" (Korean), "hang" (Hangul jamo only).
	OutputScript string `yaml:"output_script,omitempty"`

	Trigger Trigger `yaml:"trigger"`

	// Backspace selects post-commit backspace behaviour. Empty falls
	// back to an inferred default based on IM shape (see DESIGN-IME.md
	// "Per-IM defaults"). Values: "decompose", "reopen", "passthrough".
	Backspace string `yaml:"backspace,omitempty"`

	// Learning toggles per-user frequency overlay on dictionary
	// lookups. Empty falls back to inferred default (enabled iff
	// the IM has any dictionaries). Values: "enabled", "disabled".
	Learning string `yaml:"learning,omitempty"`

	// MatchClasses lets an IM declare project-specific symbol
	// equivalence sets it references in Transition.MatchClass.
	// Builtin classes (latin-letter, digit, …) are always present;
	// entries here add to or override them.
	MatchClasses map[string][]string `yaml:"match_classes,omitempty"`

	States       []State         `yaml:"states"`
	Dictionaries []DictionaryRef `yaml:"dictionaries,omitempty"`

	// Slug is the file basename (set at load time, not authored).
	Slug string `yaml:"-"`
}

// Trigger declares how the IM activates within a session.
type Trigger struct {
	// Mode is one of:
	//   "always" — IM processes every keystroke; non-matching keys
	//              pass through untouched. The dominant CJK shape.
	//   "prefix" — IM only engages after a specific keysym is seen
	//              (the Compose layer's Multi_key model).
	//   "toggle" — User toggles IM on/off with a global hotkey; the
	//              daemon manages the on/off state across sessions.
	Mode string `yaml:"mode"`

	// Hotkey is the user-facing toggle (only meaningful when
	// Mode=="toggle"). A list of modifier names ending in a key
	// name, e.g. [super, space] or [ctrl, shift, k]. Daemon default
	// applies when omitted.
	Hotkey []string `yaml:"hotkey,omitempty,flow"`

	// PrefixKeysym names the trigger keysym (only meaningful when
	// Mode=="prefix"). Defaults to "Multi_key".
	PrefixKeysym string `yaml:"prefix_keysym,omitempty"`
}

// State is one node in the IM's finite-state machine. Transitions
// are tried in declaration order; the first one whose Match[Class]
// matches the incoming symbol wins.
type State struct {
	ID string `yaml:"id"`
	// OnEnter fires when this state becomes active (after any
	// transition that lands here, before processing the next
	// symbol). Useful for updating preedit display from buffer
	// content.
	OnEnter     Action       `yaml:"on_enter,omitempty"`
	Transitions []Transition `yaml:"transitions,omitempty"`
}

// Transition is one edge out of a State. Matching: exactly one of
// Match or MatchClass should be set; an empty Match matches the
// empty symbol stream (rare — used for spontaneous transitions).
// Effects in Emit/AppendBuffer/etc. apply in declaration order;
// Goto runs last.
type Transition struct {
	// Match is a literal symbol token. Single-character symbols
	// ("a") may be written as the character; multi-character keysym
	// names ("BackSpace", "Multi_key") use their keysym form. The
	// special token "*" matches any symbol (wildcard, useful for
	// default fall-through transitions).
	Match string `yaml:"match,omitempty"`

	// MatchClass references either a builtin class name
	// (latin-letter, digit, whitespace, editing) or one declared
	// under the IM's match_classes section.
	MatchClass string `yaml:"match_class,omitempty"`

	// Action is what the transition does on a match.
	Action `yaml:",inline"`

	// Goto changes the FSM state. Empty means "stay in the current
	// state". Special value "initial" returns to the entry state.
	Goto string `yaml:"goto,omitempty"`

	// GotoIfEmpty changes state only when the buffer is empty after
	// the transition's effects ran. Lets a Backspace transition
	// pop the buffer AND exit IM mode when nothing's left.
	GotoIfEmpty string `yaml:"goto_if_empty,omitempty"`
}

// Action collects the side effects a transition (or on_enter) can
// produce: text emitted, buffer manipulation, dictionary lookup.
// Empty fields are no-ops; all fields are independent.
type Action struct {
	// Preedit sets the current preedit display. Supports template
	// substitution: ${buffer} is the IM's current buffer content;
	// ${lookup.top} is the top dictionary candidate; ${match} is
	// the symbol that triggered the transition.
	Preedit string `yaml:"preedit,omitempty"`

	// Commit emits a finalised string to the application. Supports
	// the same template substitutions as Preedit. Multiple commits
	// in one transition concatenate.
	Commit string `yaml:"commit,omitempty"`

	// AppendBuffer pushes the matched symbol onto the IM's buffer.
	AppendBuffer bool `yaml:"append_buffer,omitempty"`
	// PopBuffer removes the last symbol from the IM's buffer.
	PopBuffer bool `yaml:"pop_buffer,omitempty"`
	// ClearBuffer empties the IM's buffer.
	ClearBuffer bool `yaml:"clear_buffer,omitempty"`

	// Lookup runs a dictionary query against the current buffer.
	// Result is exposed via ${lookup.top} and ${lookup.candidate[N]}
	// in subsequent Preedit/Commit templates within this Action.
	Lookup *Lookup `yaml:"lookup,omitempty"`
}

// Lookup describes a dictionary query against a named dictionary
// declared in the IM's dictionaries: section.
type Lookup struct {
	Dictionary string `yaml:"dictionary"`
	// Rank selects the candidate-ordering strategy. Currently:
	//   "frequency"        — purely by stored frequency
	//   "frequency-prefix" — frequency, with longer prefix matches
	//                        weighted higher
	//   "user-overlay"     — user-frequency learning overlay first,
	//                        falling back to base
	Rank string `yaml:"rank,omitempty"`
}

// DictionaryRef binds a logical dictionary ID (referenced by
// Lookup.Dictionary) to an on-disk lookup table. The engine loads
// these lazily on first use.
type DictionaryRef struct {
	ID      string   `yaml:"id"`
	Source  string   `yaml:"source"`
	License string   `yaml:"license,omitempty"`
	Columns []string `yaml:"columns,omitempty,flow"`
	RankBy  string   `yaml:"rank_by,omitempty"`
}
