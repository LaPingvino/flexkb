# flexkb design

A modular alternative to xkb-config that separates *physical hardware*,
*letter arrangement*, and *language-specific characters* into three (or
four) orthogonal data dimensions. Same xkb runtime, much cleaner author
model.

## The premise

Every xkb layout collapses three independent decisions into one bundled
"layout name":

1. **Physical**: what keys actually exist on the hardware in front of you.
   ANSI (104 keys), ISO (105 keys + LSGT), JIS (109 keys), …
2. **Transformation**: what letter-arrangement convention you've learned.
   QWERTY, QWERTZ, AZERTY, Dvorak, Colemak, Workman, BÉPO, Norman, …
3. **Additions**: what language(s) you actually want to type. Polish ą/ę,
   German ä/ö/ü, Portuguese ç, Esperanto ĉ/ĝ, US-intl dead keys, …

xkb-config glues these together per-locale-per-variant. `us(dvorak-intl)`
is "ANSI + Dvorak + intl" — but you couldn't compose those words from
xkb-config; someone had to hand-write that exact symbols block, and a
similar one for `us(colemak-intl)`, `us(workman-intl)`, `carpalx-intl`,
etc. By the time you want Polish-on-Dvorak you're hoping somebody already
wrote it.

flexkb flips that. You write the three dimensions independently; the
compose engine builds the cross-product on demand.

## The four modular dimensions

### Physical (`data/physical/<name>.yaml`)

A list of XKB key codes that exist on this hardware shell. No symbol info.

```yaml
name: ANSI
keys: [TLDE, AE01..AE12, AD01..AD12, AC01..AC11, BKSL, AB01..AB10]
```

### Transformation (`data/transformations/<name>.yaml`)

Per-key level 1/level 2 base symbols. Tag with `script:` (default `latin`).

```yaml
name: Dvorak
script: latin
keys:
  AC01: { levels: [a, A] }
  AC02: { levels: [o, O] }
  AD01: { levels: [apostrophe, quotedbl] }
  # …
```

### Addition (`data/additions/<name>.yaml`)

Decorations layered on top. Three flavours, applied in this order:

#### Position overlays — fixed key codes

```yaml
overlays:
  AC10: { levels: [odiaeresis, Odiaeresis] }  # German layout puts ö here
```

Use when the glyph belongs on a specific physical key regardless of base.
Position overlays use empty-string pass-through for levels you don't want
to clobber.

#### Letter-following overlays — follow the underlying letter

```yaml
letter_overlays:
  c:
    levels: ["", "", ccedilla, Ccedilla]
    fallback: [comma]      # try these tokens if 'c' isn't at level 1
    priority: high         # win key claims over normal-priority overlays
```

Use when the glyph follows a specific Latin letter regardless of where
that letter has been arranged. **The breakthrough that makes intl work on
every Latin transformation** without hand-writing per-base variants. Also
works for Cyrillic / Hebrew / Arabic level-1 tokens — the lookup is on
the symbol token, not the script.

Priority is `high` / `""` (default) / `low`. Sorted globally across all
additions in a layout spec; higher wins. Lower-priority overlays whose
match key was claimed by a higher-priority one yield silently. Fallback
tokens are tried in order; first match wins.

#### Includes

Raw xkb include lines to append (typically `level3(ralt_switch)` so the
AltGr-level glyphs are reachable).

Tag with `scripts: [list]` (default `[latin]`). The matrix sanity test
uses this to skip script-mismatched (transformation × addition) pairs.

### Substitution (`data/substitutions/<name>.yaml`)

Character-level rewrite applied as a final pass.

```yaml
name: Latin → Cyrillic (phonetic)
map:
  a: Cyrillic_a
  b: Cyrillic_be
  # …
```

The mechanism behind phonetic non-Latin layouts. Compose any Latin
transformation, then substitute. Russian-phonetic on Dvorak, Greek-
phonetic on Colemak, all free combinations.

Prefix a name with `~` in a `substitutions:` list to apply the inverse
direction (swap source/target). Chain multiple — each substitution sees
the previous' output. Hotfix narrow overrides FIRST in the chain to
pre-empt a broader substitution.

### Layout (`data/layouts/<file>.yaml`)

The recipe. Maps the symbols/<file> name to a list of variants.

```yaml
file: us
variants:
  - name: dvorak-intl
    description: English (Dvorak, intl., with dead keys)
    physical: ansi
    transformation: dvorak
    additions: [intl]
    # substitutions: [...]   # optional
```

## Composition order

1. **Physical** seeds the key universe. Keys outside the physical's list
   are dropped with a warning (silent for known extension keys like
   LSGT/AB11 that are absent on smaller shells by design).
2. **Transformation** sets level 1/2 of every key it covers.
3. **Position overlays** (per addition, in spec order) merge over the
   composed state. Empty-string levels are pass-through.
4. **Letter-following overlays** (across *all* additions in the spec)
   sort by priority (high → default → low), then walk in order. Each
   tries primary letter, then fallbacks. Higher-priority overlays claim
   keys; lower-priority ones yield silently on already-claimed keys.
   Equal priority: later-in-spec wins.
5. **Substitutions** (in spec order) rewrite symbol tokens. Empty values
   pass through. Multi-token symbol names (`dead_grave`, `Cyrillic_a`)
   pass through untouched.
6. **Includes** from each addition are appended to the xkb output.

## Drop-in compatibility

Output is xkb_symbols files in the format libxkbcommon parses. flexkb is
a generator, not a runtime replacement.

`flexkb build --xkb /usr/share/X11/xkb <out>` produces a complete xkb
tree: every modular symbols file is composed; every other file is copied
verbatim from upstream; rules registries are patched to advertise the new
variants.

`flexkb dropin-check <out> <upstream>` is the load-bearing test: it
fails if any upstream file, any upstream `xkb_symbols` block, or any
upstream rules-registry layout/variant is missing from `<out>`. The
PKGBUILD's `check()` runs it, so a regression in the generator fails
the package build, not the install.

Variants we own modularly REPLACE the upstream version of those variants
(within the same symbols file). Upstream variants we don't own are
passed through verbatim by re-emitting their original source. So a
user's current `us(dvorak-intl)` keeps working whether we've decomposed
it modularly or not.

## User-level data

`flexkb` walks an ordered data search path. Highest priority first:

1. `$XDG_CONFIG_HOME/flexkb/data` (defaults to `~/.config/flexkb/data`)
2. `$XDG_DATA_HOME/flexkb/data`
3. `./data` (when running from a repo)
4. `/usr/share/flexkb/data` (system install)
5. `<binary-dir>/data` (tarball installs)

Same-name files in a higher-priority directory shadow lower ones; new
files just add. `flexkb paths` shows the resolved chain.

## Testing

- `internal/compose`: unit tests for merge / overlay / substitution / etc.
- `internal/xkbparser`: parser unit tests + `FuzzParse` (run with
  `go test -fuzz=FuzzParse ./internal/xkbparser`).
- `internal/dropincheck`: tests for missing-file / missing-variant /
  missing-rules-registry detection.
- `internal/rulespatch`: tests for XML + .lst patching.
- `tests/matrix_test.go`: walks every script-compatible (transformation
  × addition) pair, runs sanity checks on the composed result. Informs
  on dead overlays, addition-clobbers-base, etc.
- `tests/coverage_test.go`: reports per-variant key diffs against the
  system xkb tree as a quality bar.

## Future opportunities (not yet implemented)

### Indic script generalization
The seven Indic substitutions we ship (Devanagari, Bengali, Tamil,
Telugu, Kannada, Malayalam, Gujarati, Gurmukhi, Oriya) all share the
Brahmic phonological structure: same consonant series by point of
articulation (velars k-kh-g-gh-ṅ, palatals c-ch-j-jh-ñ, etc.), same
vowel inventory (a, ā, i, ī, u, ū, e, ai, o, au), same virama (◌्),
same matra patterns. Each script is the same logical mapping with
different Unicode codepoint ranges.

A future "Indic phonetic template + per-script codepoint table" model
would collapse seven 60-line substitution files into one template plus
seven ~30-line codepoint tables. The same pattern could apply to Slavic
Cyrillic dialects (Bulgarian, Ukrainian, etc.) once the base phonetic
substitution mechanism is widely-shared. Worth exploring once the
broader xkb-replacement story is more battle-tested.

### Vietnamese without dead-key stacking
The current vietnamese-tone addition makes ư/ơ/ă/â/ê/ô/đ directly
accessible on AltGr (no horn-then-vowel composition needed) AND places
tone-mark dead keys on Telex-muscle-memory positions (AltGr+f for
huyền, AltGr+s for sắc, etc.). A toned vowel is still inherently
"base vowel + tone mark" — that's how Vietnamese works — but every
non-toned Vietnamese letter is one keystroke.

### Options modularization
xkb's runtime "options" (Caps→Ctrl, AltGr placement choice, compose-key
location, kill-X-server keybind) are currently pure passthrough.
Modelling them as their own module type would let users compose-and-
pick the option set the same way they pick a transformation.

### Compose tables, key types, group switching
See the README for the unhandled long-tail of xkb features. Each is
non-trivial; none essential for the core thesis.
