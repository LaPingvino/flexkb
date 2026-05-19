# flexkb IME design

A design document for extending flexkb beyond static xkb maps into the
*input method* tier — multi-keystroke conversion, dictionary lookup,
stateful preedit. The same "modular composition over hand-written
per-language files" promise carried into IME territory.

**Status**: design draft. Nothing here is implemented yet. This
document is the contract; code that doesn't match the contract should
fail review.

## The premise

Today flexkb composes static keyboard maps out of four modular
dimensions (Physical × Transformation × Additions × Substitutions),
with a fifth (Compose) added recently for multi-key stateless
sequences. xkb runs the result.

What's missing: anything *stateful*. Hangul syllable assembly,
Vietnamese tone composition, Chinese Pinyin → Hanzi dictionary lookup,
Japanese romaji → kana → kanji conversion — every conversion that
needs intermediate visual state ("preedit") and backspace-aware
correction lives outside xkb entirely, in IMEs like ibus-hangul,
ibus-libpinyin, fcitx5-mozc, ibus-m17n.

The IME ecosystem repeats the same per-language-per-implementation
fragmentation that flexkb already fixed at the layout level. One
engine + N declarative rule files would replace it the same way one
compose pipeline + N transformation/addition files replaced
xkeyboard-config.

## The premise we adopt

**Every layer in flexkb is the same kind of thing:** a declarative
rule layer that transforms an input event stream into an output event
stream. The layers differ only in how much state they're allowed to
carry across events.

```
keypress event
  ↓ Physical          : keycode (which physical key)
  ↓ Transformation    : keycode → base symbol      (stateless, position-keyed)
  ↓ Additions         : level overlay              (stateless, position+level-keyed)
  ↓ Substitutions     : symbol rewrite             (stateless, character-keyed)
  ↓ Compose           : k₁…kₙ → string             (finite state, no preedit)
  ↓ InputMethod       : k₁…kₙ → string + state    (unbounded state, preedit, backspace)
result string → application
```

Each layer's *input* is the previous layer's *output*. Crucially, the
IME tier sees a symbol stream, not raw keycodes — so a Pinyin IME
running on top of `colemak+intl` sees the same `n,i,h,a,o` as it would
on top of `qwerty`. One IM rule file works across every Latin base.

The InputMethod layer subsumes the rule shape of every layer below
it. A Compose chain is a one-state IM with no preedit. A
single-transition state machine is a Substitution. The data model
is one schema; layers "promote" by populating richer fields.

## Data model

### Directory layout

```
data/
  physical/        # existing
  transformations/ # existing
  additions/       # existing
  substitutions/   # existing
  compose/         # existing — stateless multi-key sequences
  inputmethods/    # NEW — stateful conversion + dictionaries
  dictionaries/    # NEW — large lookup tables referenced by IMs
```

`compose/` and `inputmethods/` are kept as separate directories not
because the format differs, but because the *operational contract*
differs: Compose chains can be serialised to X11 Compose files and
consumed today by libX11/libxkbcommon; InputMethods require the
flexkb Wayland daemon (or an `ibus-m17n`-compatible export). Keeping
them separate makes "what works on a vanilla Linux box right now"
greppable.

A Compose chain *can* be expressed as an InputMethod (it's the
degenerate one-state case). When that's done, the file moves
directories and its consumer mechanism changes — but its YAML
content barely changes.

### InputMethod YAML schema

```yaml
name: zh-pinyin
description: Chinese (Pinyin → Hanzi with dictionary)

# input_layer declares which symbol stream this IM expects. The
# validator uses this to reject nonsense stacks ("Pinyin IM on top
# of a Cyrillic transformation" is incoherent). One of:
#   latin — any Latin-output stack (qwerty, dvorak, colemak, …)
#   cyrillic, greek, hebrew, arabic, devanagari, …
#   any — accept whatever the lower layers produce (rare; used by
#         layout-agnostic IMs like emoji pickers)
input_layer: latin

# Output script tag — what the IM commits. Used by the validator
# to check whether stacking this IM provides the characters a
# layout's locale requires.
output_script: hani

# Trigger declares how the IM activates. One of:
#   always   — IM is on for every keystroke (the symbol stream is
#              always processed; non-matching keys pass through)
#   prefix   — IM only engages after a specific keysym (Compose's
#              <Multi_key> trigger lives here)
#   toggle   — user toggles IM on/off with a global hotkey (Mod+Space
#              by convention); flexkb daemon manages the state
trigger:
  mode: toggle
  hotkey: [super, space]   # optional; daemon default if omitted

# states are the FSM nodes. Every IM has at least an "initial"
# state. Transitions consume symbols from the input stream and
# either commit output, update preedit, or change state.
states:
  - id: initial
    transitions:
      # Match a sequence of input symbols. Match shorthand:
      #   "n"        — single literal symbol
      #   "ni"       — concatenated literals (each char is one symbol)
      #   [n, i]     — explicit list (use for multi-char keysyms)
      #   "*"        — any single symbol (wildcard)
      - match: "n"
        emit: { preedit: "n" }
        goto: pinyin-buffer

  - id: pinyin-buffer
    on_enter: { preedit: "${buffer}" }
    transitions:
      # Letter pressed → append to buffer, keep showing preedit
      - match_class: pinyin-letter
        append_buffer: true
      # Space → look up buffer in dictionary, commit best match
      - match: space
        lookup:
          dictionary: zh-pinyin-cc-cedict
          rank: frequency-prefix
        commit: "${lookup.top}"
        clear_buffer: true
        goto: initial
      # Digit 1..9 → commit Nth candidate
      - match_class: digit
        commit: "${lookup.candidate[${match}]}"
        clear_buffer: true
        goto: initial
      # Backspace → drop last char of buffer; if empty, exit IM state
      - match: BackSpace
        pop_buffer: true
        goto_if_empty: initial
      # Escape → cancel; buffer discarded, IM returns to initial
      - match: Escape
        clear_buffer: true
        goto: initial

# dictionary section binds lookup-table references this IM needs.
# Files live under data/dictionaries/ — pluggable, language-specific,
# usually large (multi-MB), so kept out of git or in a separate repo
# / package for licensing reasons (e.g. CC-CEDICT, JMdict).
dictionaries:
  - id: zh-pinyin-cc-cedict
    source: data/dictionaries/cc-cedict.tsv
    license: CC-BY-SA-4.0
    columns: [pinyin, simplified, traditional, frequency]
    rank_by: frequency
```

The Compose layer's schema (already implemented) is exactly this
schema with `states: [single-state]`, no `dictionaries:`, no
`trigger.mode == toggle`, and `output_script` omitted. The same
parser loads both directories.

### Built-in match classes

`match_class` references symbol equivalence sets the engine ships
with:

| Class | Members |
|---|---|
| `latin-letter` | a–z, A–Z |
| `latin-vowel` | a, e, i, o, u (case-insensitive) |
| `pinyin-letter` | a–z (Pinyin only ever uses lowercase Latin) |
| `digit` | 0–9 |
| `whitespace` | space, Tab |
| `editing` | BackSpace, Delete, Left, Right, Home, End |
| `(custom)` | declared in the IM file under `match_classes:` |

Custom classes let an IM file declare project-specific sets without
hardcoding them in the engine.

## Worked example: Pinyin IME for a Russian typist

The "IME consumes the lower layers' output" rule pays off in stacks
nobody would author by hand. A native Russian speaker who types on a
ЙЦУКЕН Cyrillic keyboard and wants to input Chinese:

```yaml
# data/layouts/ru.yaml
- name: pinyin-via-russian
  description: Chinese (Pinyin input from a Russian Cyrillic keyboard)
  physical: iso
  transformation: ycuken                # types Cyrillic letters
  substitutions: [~latin-cyrillic-phonetic]  # invert: cyrillic → latin
  inputmethods: [zh-pinyin]
```

Data flow on one keystroke:

```
user presses <AC02>  (the physical position of 'ы' on ЙЦУКЕН)
  ↓ Transformation:  <AC02> → Cyrillic_yeru
  ↓ Substitution ~latin-cyrillic-phonetic:  Cyrillic_yeru → y
  ↓ Compose:         no Multi_key prefix, passes through
  ↓ InputMethod zh-pinyin:  buffer = "y", preedit shown
```

The Pinyin IM never knew the keystroke originated from a Cyrillic
position. The `~latin-cyrillic-phonetic` substitution (inverse
direction of the existing forward map — already supported via the
`~` prefix) feeds it a Latin stream. The IM file is unchanged; the
substitution file is unchanged; the layout recipe is the only new
artifact.

Same logic generalises:

| Layout recipe (transformation + substitution + IM) | Effect |
|---|---|
| `ycuken + ~latin-cyrillic + zh-pinyin` | Pinyin Chinese from a Russian keyboard |
| `inscript + ~latin-devanagari + zh-pinyin` | Pinyin Chinese from an InScript Indian keyboard |
| `arabic + ~latin-arabic-phonetic + ja-romaji` | Japanese romaji from an Arabic keyboard |
| `dvorak + cs-pinyin-ime` *(layout-coupled toggle)* | Standard QWERTY-Pinyin for a Dvorak typist |

None of those combinations need bespoke files. The validator
(`Compositional validation`, next section) confirms that each stack's
*produced character set* covers the target locale's *required
character set* — or reports exactly which characters and which extra
layers would close the gap.

## Compositional validation

The matrix sanity test (`tests/matrix_test.go`) already filters
incompatible layer pairs at the xkb tier via `script:` tags. This
extends naturally upward:

```
target locale requires: { a-z, ī, ñ, क, क्ष, ज्ञ, … }

layer stack produces:
    { a-z, ī, ñ, … }                from L1–L4 of qwerty+intl
  ∪ { क, ख, ग, … }                  from latin-devanagari-phonetic substitution
  ∪ { क्ष, ज्ञ }                   from latin-devanagari-phonetic compose chain
  ∪ { precomposed-syllables, … }   from devanagari-conjunct-im dictionary

→ produced ⊇ required  ✓  combination is complete
→ produced ⊉ required  ✗  list missing chars + propose layers that supply them
```

Validation surfaces three classes of problem:

1. **Coherence**: an IM whose `input_layer` doesn't match the
   transformation's output script. Pinyin (Latin) × ЙЦУКЕН (Cyrillic)
   is incoherent and the validator rejects it before composition.
2. **Completeness**: the layer stack misses characters the locale
   needs. Validator lists them and proposes a layer that would
   supply each (e.g. "Ç missing — add the `polish-on-intl` addition
   or the `cs` substitution").
3. **Redundancy**: two layers in the stack supply the same character
   via different paths. Warned but not blocked — sometimes redundancy
   is intentional (e.g. ç on both AltGr and a Compose sequence).

Validator hooks:

```sh
flexkb validate us hindi-phonetic            # full report for one variant
flexkb validate --all                         # matrix-sanity across every layout file
flexkb validate --missing-only zh zh-pinyin   # only what's MISSING from a stack
```

## Engine — the Wayland IME daemon

A new binary `flexkb-imed` (or `flexkb daemon` subcommand) speaks the
Wayland `input-method-v2` protocol. It:

1. Reads `data/inputmethods/*.yaml` at startup (and on SIGHUP).
2. Reads `data/compose/*.yaml` and loads each as a one-state IM.
3. Loads dictionaries lazily on first use.
4. Connects to the Wayland display, advertises one IME engine per
   loaded IM file.
5. Maintains per-text-input session state: which IM is active, the
   FSM state, the preedit buffer, the dictionary lookup cursor.
6. Forwards keystrokes through the matching IM's FSM, emitting
   `preedit_string` / `commit_string` events back to the compositor.

**Mode** — the daemon supports both:

- Per-layout coupling: when the active xkb layout changes (signalled
  by the compositor via the standard layout-change channel), the
  daemon auto-loads the IMs listed in the matching layout file's
  `inputmethods:` field. Hindi phonetic active → Devanagari IM loaded
  with no user action.
- Manual selection: a global hotkey (default Mod+Space) cycles
  through loaded IMs. Standard ibus/fcitx5 UX. Used for CJK where
  one Latin layout serves many target scripts.

**Process model**: one daemon per user session, started by the
desktop session (`systemd --user` unit shipped with the package).
The GUI configurator (`flexkb gui`) talks to the daemon over a
Unix socket for live state inspection — preedit content, candidate
list, validation diagnostics. Configurator and daemon are separate
binaries so the daemon stays tiny and the GUI can be a heavier
webview.

**X11**: out of scope for the initial implementation. If demand
materialises, an `ibus-m17n`-shaped backend can be added later that
exposes the same IM files via the existing ibus engine API.

## m17n-mim interop

`m17n-db` ships rule files (`.mim` extension) for ~70 input methods
including Hangul, Vietnamese telex/VNI, every Indic script, Thai,
Tibetan, etc. These files are a near-perfect match for the schema
above modulo notation. The strategy:

1. **Import**: convert m17n `.mim` files to flexkb YAML at build
   time, vendor the output under `data/inputmethods/`. Run upstream
   m17n-db as a build dependency; track its release cadence.
2. **Export**: emit `.mim` from a flexkb YAML file so users running
   `ibus-m17n` on a non-flexkb-daemon system can still get the
   composition rules. Lossless for the structured subset (~95% of
   real-world IMs); exotic mim files using the full Lisp action
   language need a manual port — log them at conversion time, don't
   silently drop.
3. **Coexist**: where m17n already has a good rule file, vendor it
   straight. flexkb-authored IMs are for cases where m17n is
   missing, incomplete, or wrong.

## Packaging

The IME tier matures the question of "is xkeyboard-config replacement
really the right default for flexkb?" Probably not, once IM works on
top of stock xkb data:

| Package | Role | Conflicts with |
|---|---|---|
| `flexkb` | IME daemon + GUI configurator. Reads `data/compose/`, `data/inputmethods/`, `data/dictionaries/`. Coexists peacefully with anything. | nothing |
| `flexkb-xkb` | Today's xkb tree — drop-in xkeyboard-config replacement. All `data/physical/transformations/additions/substitutions/layouts/*.yaml` + `flexkb build`. | `xkeyboard-config` |
| `flexkb-data-cjk` | Dictionaries: CC-CEDICT, JMdict, … Split out for licensing and download size. | nothing |

Default install is `flexkb` alone. Users who want the xkeyboard-config
replacement opt into `flexkb-xkb`. Users who want CJK dictionaries
install `flexkb-data-cjk`. Same data layout under `/usr/share/flexkb/data/`
regardless of which package shipped it.

## Per-IM defaults and user overrides

Two knobs that differ by IM type are configurable but auto-default to
whatever makes sense for the chosen layout. The IM file declares
defaults; the user can override in `~/.config/flexkb/imrc.yaml` per-IM
or globally. The GUI surfaces both knobs with the auto-detected default
pre-selected, so most users never touch them.

### Backspace semantics

Three modes, expressed in the IM file as `backspace:`:

| Mode | Meaning | Auto-default for |
|---|---|---|
| `decompose` | During preedit, Backspace decomposes the in-flight cluster (e.g. Hangul ㄱ+ㅏ+ㄴ → ㄱ+ㅏ → ㄱ); after commit, Backspace passes to the app. | Stateful syllable assemblers: Hangul, Vietnamese tone composers, Indic conjunct assemblers. |
| `reopen` | After commit, the first Backspace undoes the last commit and reopens its preedit so the user can pick a different candidate. | Dictionary-lookup IMs: Pinyin, Wubi, kana-kanji — where the user often realises the wrong candidate was chosen. |
| `passthrough` | Backspace is always handed to the app; the IM never intercepts. | Stateless IMs (Compose chains promoted to one-state machines), and anything where the user explicitly prefers "Backspace = delete a character, full stop". |

The engine still always handles Backspace *during active preedit* — that
case is FSM-internal, not a configuration. The knob controls only the
"post-commit Backspace" decision.

### Learning

User-frequency learning improves candidate ranking over time. Two
modes, expressed as `learning:`:

| Mode | Meaning | Auto-default for |
|---|---|---|
| `enabled` | Commits update a per-user frequency overlay at `~/.local/share/flexkb/learning/<im>.tsv`. Lookups rank overlay-frequency above base-dictionary frequency. | Any IM with a `dictionaries:` block: Pinyin, Wubi, kana-kanji. |
| `disabled` | No overlay; ranking is purely from the static dictionary (or there is no dictionary). | Stateless rule IMs (Compose-promoted, Esperanto x-system, Vietnamese telex, Hangul). |

`enabled` is fully reversible — the overlay file can be deleted by the
user at any time; the engine continues with base-dictionary ranking.
The GUI offers a "reset learning" button per IM.

### Resolution order

When the engine loads an IM for a session, the active configuration is
computed as:

```
1. Built-in default (decompose / reopen / passthrough per the table above)
2. Override from the IM file's `backspace:` / `learning:` field
3. Override from ~/.config/flexkb/imrc.yaml (per-IM section)
4. Override from a runtime GUI toggle (persisted back to imrc.yaml on change)
```

Each level only overrides what it explicitly sets — partial overrides
are fine.

## Open questions

1. **Latency budget.** Wayland IM events are synchronous-ish; how
   long can `flexkb-imed` spend on a dictionary lookup before the
   compositor notices? Measure on a baseline reference IM (Pinyin
   with CC-CEDICT, ~120k entries) and set a hard ceiling.
2. **State persistence across compositor restarts.** Probably no —
   FSM state is per-session. But user preferences (which IM is
   active for which xkb variant, which candidate they picked at
   index 1 vs 2 historically) persist in `imrc.yaml` and the
   learning overlay — those are config concerns, not daemon
   concerns.

## Implementation order

1. Engine — schema parser, FSM evaluator, dictionary loader. Pure Go,
   no daemon yet. Unit-tested with synthetic IMs.
2. Validation — extend `tests/matrix_test.go` and add `flexkb validate`.
3. m17n import pipeline — convert a handful of mim files, exercise
   the engine on real data.
4. Wayland daemon — `input-method-v2` protocol binding, session
   management.
5. GUI integration — live preedit display, validation panel.
6. Packaging split — separate `flexkb-xkb` from the base `flexkb`.

Each step is independently usable: after step 1 you can `flexkb run-im
<name> <stream>` and see commits; after step 2 you can audit
combinations; after step 3 you have a real-data test set; after step
4 you can actually type with it.
