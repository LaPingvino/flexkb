package inputmethod

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/lapingvino/flexkb/internal/model"
)

// FromCompose converts a flexkb ComposeChain into an InputMethod by
// constructing a trie of states: one state per distinct prefix of
// the chain's sequences, with leaf transitions that commit the
// composed output and return to initial. Unmatched symbols at any
// intermediate state discard the in-flight prefix (matching X-Compose
// semantics — an unknown sequence after the prefix key is a no-op).
//
// This is the engine-side proof that the IME tier subsumes the
// Compose tier: feeding the resulting InputMethod a sequence like
// {x} (after the chain's prefix has been consumed) produces the same
// committed string as the X-Compose file would.
//
// The chain's prefix keysym is recorded in the IM's Trigger so the
// daemon (or a test harness) knows when to start routing symbols
// through the FSM. The FSM itself starts at the post-prefix state —
// callers feed it the symbols that follow the prefix, not the prefix
// itself.
func FromCompose(c model.ComposeChain) (*InputMethod, error) {
	if len(c.Sequences) == 0 {
		return nil, fmt.Errorf("compose chain %q has no sequences", c.Name)
	}
	prefix := c.Prefix
	if prefix == "" {
		prefix = "Multi_key"
	}

	// Build a prefix trie. trie[pathKey] holds the transitions out
	// of the state representing that path. The empty path is the
	// initial state.
	type node struct {
		// children[symbol] = pathKey of the destination state
		children map[string]string
		// emit (set only on leaf nodes): the composed output string
		emit string
		// emitComment (set only on leaf nodes): a comment string,
		// preserved into the IM for debug/preview output
		emitComment string
	}
	nodes := map[string]*node{"": {children: map[string]string{}}}

	for _, seq := range c.Sequences {
		if len(seq.Input) == 0 {
			return nil, fmt.Errorf("compose chain %q sequence %q has no input", c.Name, seq.ID)
		}
		if len(seq.Output) == 0 {
			return nil, fmt.Errorf("compose chain %q sequence %q has no output", c.Name, seq.ID)
		}
		output, err := renderOutput(seq.Output)
		if err != nil {
			return nil, fmt.Errorf("compose chain %q sequence %q: %w", c.Name, seq.ID, err)
		}
		path := ""
		for i, sym := range seq.Input {
			next := path + "\x00" + sym
			if _, ok := nodes[path].children[sym]; !ok {
				nodes[path].children[sym] = next
				nodes[next] = &node{children: map[string]string{}}
			}
			if i == len(seq.Input)-1 {
				leaf := nodes[nodes[path].children[sym]]
				leaf.emit = output
				leaf.emitComment = composeComment(seq)
			}
			path = nodes[path].children[sym]
		}
	}

	// Emit states. The empty-path node is "initial"; all others get
	// auto-generated ids derived from their input prefix. Stable
	// ordering for deterministic output.
	stateID := func(path string) string {
		if path == "" {
			return "initial"
		}
		// pathKey is "\x00sym1\x00sym2…"; strip leading null and join
		// with underscores for a readable state name.
		raw := strings.TrimPrefix(path, "\x00")
		return "prefix_" + strings.ReplaceAll(raw, "\x00", "_")
	}

	var ids []string
	for path := range nodes {
		ids = append(ids, path)
	}
	sort.Strings(ids)

	im := &InputMethod{
		Name:        c.Name,
		Description: c.Description,
		Slug:        c.Slug,
		Trigger: Trigger{
			Mode:         "prefix",
			PrefixKeysym: prefix,
		},
	}

	for _, path := range ids {
		n := nodes[path]
		st := State{ID: stateID(path)}
		// Sort children by symbol for stable transition order.
		var syms []string
		for sym := range n.children {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			child := nodes[n.children[sym]]
			tr := Transition{Match: sym}
			if child.emit != "" {
				// Leaf — commit the composed output and return.
				tr.Action.Commit = child.emit
				tr.Goto = "initial"
			} else {
				// Intermediate — advance into the trie.
				tr.Goto = stateID(n.children[sym])
			}
			st.Transitions = append(st.Transitions, tr)
		}
		// Wildcard fallback on intermediate states: unmatched symbol
		// discards the partial sequence and returns to initial. The
		// terminal "initial" state doesn't need a fallback since
		// unmatched symbols there naturally passthrough.
		if path != "" {
			st.Transitions = append(st.Transitions, Transition{
				Match: "*",
				Goto:  "initial",
			})
		}
		im.States = append(im.States, st)
	}

	// Put the initial state first — NewSession enters States[0] and
	// the wider system assumes "initial" is the entry point.
	sort.SliceStable(im.States, func(i, j int) bool {
		return im.States[i].ID == "initial" && im.States[j].ID != "initial"
	})

	return im, nil
}

// renderOutput converts a list of Uxxxx codepoints into the
// committed UTF-8 string. Mirrors the equivalent logic in
// composewriter but stripped of X-Compose-specific quoting (no
// backslash escapes — the result string is what the application
// receives, not what the .XCompose file contains).
func renderOutput(codepoints []string) (string, error) {
	var b strings.Builder
	for _, cp := range codepoints {
		if len(cp) < 2 || (cp[0] != 'U' && cp[0] != 'u') {
			return "", fmt.Errorf("output token %q: not a U-escape", cp)
		}
		v, err := strconv.ParseUint(cp[1:], 16, 32)
		if err != nil {
			return "", fmt.Errorf("output token %q: %w", cp, err)
		}
		b.WriteRune(rune(v))
	}
	return b.String(), nil
}

// composeComment formats a one-line debug string from the sequence's
// id/category/description so the generated IM remains readable when
// dumped for inspection or round-tripped to X-Compose.
func composeComment(s model.ComposeSequence) string {
	parts := []string{}
	if s.ID != "" {
		parts = append(parts, s.ID)
	}
	if s.Category != "" {
		parts = append(parts, s.Category)
	}
	if s.Description != "" {
		parts = append(parts, s.Description)
	}
	return strings.Join(parts, " — ")
}
