package mimimport

import (
	"fmt"
	"strings"

	"github.com/lapingvino/flexkb/internal/inputmethod"
)

// Convert parses a mim source text and produces a flexkb
// InputMethod plus a list of warnings. Warnings describe pieces of
// the original file we couldn't represent in the structured subset
// (action forms beyond literal output, branch/pushback/cond
// constructs, etc.). The caller can decide whether to ship the
// converted IM anyway or refuse based on warning content.
//
// Files using unsupported forms still parse — Convert tells you
// which lines fell off the wagon so a human can decide between
// (a) hand-porting that mim file, (b) accepting the lossy result,
// (c) waiting for flexkb's evaluator to grow the missing capability.
func Convert(src string) (*inputmethod.InputMethod, []string, error) {
	nodes, err := ParseSexp(src)
	if err != nil {
		return nil, nil, err
	}

	var warnings []string
	maps := map[string]map[string][]string{} // mapName → input → output rune sequence
	stateMaps := map[string][]string{}       // stateName → []mapNames used in that state
	stateOrder := []string{}
	var name, description, title string

	for _, n := range nodes {
		if n.Kind != "list" || len(n.Children) == 0 || n.Children[0].Kind != "symbol" {
			continue
		}
		head := n.Children[0].Sym
		switch head {
		case "input-method":
			// Expected shape: (input-method LANG NAME [EXTRA-ID])
			if len(n.Children) >= 3 {
				lang := symOrEmpty(n.Children[1])
				short := symOrEmpty(n.Children[2])
				if lang != "" && short != "" {
					name = lang + "-" + short
				}
			}

		case "description":
			if len(n.Children) >= 2 && n.Children[1].Kind == "string" {
				description = n.Children[1].Str
			}

		case "title":
			if len(n.Children) >= 2 && n.Children[1].Kind == "string" {
				title = n.Children[1].Str
			}

		case "map":
			// (map (MAPNAME (INPUT OUTPUT...) ...) (MAPNAME ...) ...)
			for _, mn := range n.Children[1:] {
				if mn.Kind != "list" || len(mn.Children) == 0 || mn.Children[0].Kind != "symbol" {
					continue
				}
				mapName := mn.Children[0].Sym
				rules := map[string][]string{}
				for _, ruleNode := range mn.Children[1:] {
					if ruleNode.Kind != "list" || len(ruleNode.Children) < 2 {
						continue
					}
					input, output, warn, ok := extractRule(ruleNode)
					if warn != "" {
						warnings = append(warnings, fmt.Sprintf("map %q: %s", mapName, warn))
					}
					if !ok {
						continue
					}
					rules[input] = output
				}
				maps[mapName] = rules
			}

		case "state":
			// (state (STATENAME (MAPNAME [ACTION]) ...) ...)
			for _, sn := range n.Children[1:] {
				if sn.Kind != "list" || len(sn.Children) == 0 || sn.Children[0].Kind != "symbol" {
					continue
				}
				stateName := sn.Children[0].Sym
				var usedMaps []string
				for _, entry := range sn.Children[1:] {
					if entry.Kind == "list" && len(entry.Children) >= 1 && entry.Children[0].Kind == "symbol" {
						usedMaps = append(usedMaps, entry.Children[0].Sym)
					} else if entry.Kind == "symbol" {
						usedMaps = append(usedMaps, entry.Sym)
					}
				}
				if _, exists := stateMaps[stateName]; !exists {
					stateOrder = append(stateOrder, stateName)
				}
				stateMaps[stateName] = usedMaps
			}

		case "title-prefix-required", "variable", "command", "macro", "module":
			// Forms we don't handle but that aren't blockers — note them.
			warnings = append(warnings, fmt.Sprintf("ignoring (%s ...) — not handled by v1 importer", head))
		}
	}

	if name == "" {
		return nil, warnings, fmt.Errorf("no (input-method LANG NAME) form found")
	}
	if len(stateOrder) == 0 {
		// No (state ...) form — treat all maps as if a single
		// "init" state references all of them. Some mim files
		// elide state when only one map exists.
		stateOrder = []string{"init"}
		for mapName := range maps {
			stateMaps["init"] = append(stateMaps["init"], mapName)
		}
	}

	// Build the InputMethod. Each m17n state becomes a flexkb FSM
	// initial trie root; for v1 we collapse to a single trie state
	// graph whose root is "initial" — multi-state m17n IMs that
	// switch states mid-input aren't fully supported, the warning
	// tells the user.
	if len(stateOrder) > 1 {
		warnings = append(warnings, fmt.Sprintf("%d states found (%v); v1 importer flattens to one — state transitions not preserved", len(stateOrder), stateOrder))
	}
	rootState := stateOrder[0]
	usedMaps := stateMaps[rootState]

	im := &inputmethod.InputMethod{
		Name:        name,
		Description: collapseDescription(description, title),
		Trigger: inputmethod.Trigger{
			Mode: "always",
		},
	}

	// Build a trie of all (input → output) pairs from all maps
	// active in the root state. Two rules with the same input but
	// different outputs: the LAST one wins, with a warning (mim
	// uses ordering to express precedence; flexkb's first-match
	// FSM expresses it via transition order, which our trie
	// builder collapses).
	type node struct {
		children map[rune]int
		commit   string
	}
	nodes_ := []node{{children: map[rune]int{}}}
	for _, mapName := range usedMaps {
		rules, ok := maps[mapName]
		if !ok {
			continue
		}
		for input, output := range rules {
			cur := 0
			for _, r := range input {
				next, exists := nodes_[cur].children[r]
				if !exists {
					nodes_ = append(nodes_, node{children: map[rune]int{}})
					next = len(nodes_) - 1
					nodes_[cur].children[r] = next
				}
				cur = next
			}
			if existing := nodes_[cur].commit; existing != "" && existing != strings.Join(output, "") {
				warnings = append(warnings, fmt.Sprintf("input %q has conflicting outputs %q and %q — keeping latter", input, existing, strings.Join(output, "")))
			}
			nodes_[cur].commit = strings.Join(output, "")
		}
	}

	// Emit states from the trie.
	stateID := func(i int) string {
		if i == 0 {
			return "initial"
		}
		return fmt.Sprintf("s%d", i)
	}
	for i, n := range nodes_ {
		st := inputmethod.State{ID: stateID(i)}
		// Deterministic order — sort runes for stable output.
		runes := make([]rune, 0, len(n.children))
		for r := range n.children {
			runes = append(runes, r)
		}
		// Insertion-order safe sort (avoids importing sort just
		// for runes — small lists, manual sort is fine).
		for a := 1; a < len(runes); a++ {
			for b := a; b > 0 && runes[b-1] > runes[b]; b-- {
				runes[b-1], runes[b] = runes[b], runes[b-1]
			}
		}
		for _, r := range runes {
			child := nodes_[n.children[r]]
			tr := inputmethod.Transition{Match: string(r)}
			if child.commit != "" {
				tr.Action.Commit = child.commit
				tr.Goto = "initial"
			} else {
				tr.Goto = stateID(n.children[r])
			}
			st.Transitions = append(st.Transitions, tr)
		}
		// Non-root states: unmatched suffix discards the partial.
		if i != 0 {
			st.Transitions = append(st.Transitions, inputmethod.Transition{
				Match: "*",
				Goto:  "initial",
			})
		}
		im.States = append(im.States, st)
	}

	return im, warnings, nil
}

// extractRule walks a single (INPUT OUTPUT...) rule and returns
// the input key sequence (string of characters typed), the output
// rune sequence (each entry is a single-character or multi-
// character string), a warning if the rule used a form the v1
// importer couldn't fully represent, and ok=false to skip the
// rule entirely.
func extractRule(rule Node) (input string, output []string, warn string, ok bool) {
	if len(rule.Children) < 2 {
		return "", nil, "", false
	}
	inNode := rule.Children[0]
	switch inNode.Kind {
	case "string":
		input = inNode.Str
	case "char":
		input = inNode.Str
	case "symbol":
		// Key-sequence symbols ("Tab", "C-x") — v1 doesn't model
		// modifier keysyms inside inputs.
		return "", nil, fmt.Sprintf("non-character input %q skipped (modifier keysyms unsupported)", inNode.Sym), false
	default:
		return "", nil, fmt.Sprintf("unrecognised input node kind %q", inNode.Kind), false
	}
	for _, on := range rule.Children[1:] {
		switch on.Kind {
		case "string":
			output = append(output, on.Str)
		case "char":
			output = append(output, on.Str)
		case "symbol":
			// (insert SOMETHING) and friends — action forms.
			return "", nil, fmt.Sprintf("non-literal output containing symbol %q skipped (action forms unsupported)", on.Sym), false
		case "list":
			return "", nil, "non-literal output containing nested form skipped (action forms unsupported)", false
		}
	}
	if len(output) == 0 {
		return "", nil, "", false
	}
	return input, output, "", true
}

func symOrEmpty(n Node) string {
	if n.Kind == "symbol" {
		return n.Sym
	}
	return ""
}

func collapseDescription(desc, title string) string {
	desc = strings.TrimSpace(desc)
	if title != "" && !strings.Contains(desc, title) {
		if desc == "" {
			return title
		}
		return title + " — " + desc
	}
	return desc
}
