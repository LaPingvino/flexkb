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
Wayland `input-method-v2` protocol AND grabs raw keyboard events via
the protocol's keyboard-grab extension. The daemon owns the **entire**
layer stack — not just the IME tier — so removing the xkb static-data
dependency stops being a "step 6 packaging concern" and becomes the
v1 architecture.

```
                ┌─────────────────────────────────────────┐
                │  flexkb-imed                            │
keycode ──grab──▶│  Physical                             │
                │   ↓ Transformation                      │
                │   ↓ Additions                           │
                │   ↓ Substitutions                       │
                │   ↓ Compose                             │
                │   ↓ InputMethod                         │
                │  result string                          │
                └────────────────┬────────────────────────┘
                                 │  commit_string / preedit_string
                                 ▼
                          Wayland compositor → application
```

The daemon's data input is the same `data/*.yaml` tree the static
build pipeline reads, evaluated at runtime. The compositor sees a
purely-text input from the daemon for any focused text field where
the user has flexkb's IM active. libxkbcommon stays in the
compositor (it still handles compositor shortcuts, login-shell
input, etc.) but is no longer in the user's text-input path.

Behaviour, in order of operation:

1. Reads `data/{physical,transformations,additions,substitutions,compose,inputmethods}/*.yaml`
   at startup (and on SIGHUP).
2. Loads dictionaries lazily on first use.
3. Connects to the Wayland display, advertises one IME engine per
   loaded layout recipe. The "engine" represents a fully composed
   stack — `us+dvorak+intl` is one engine, `in+hindi-phonetic` is
   another. The user picks a stack the same way they'd pick an xkb
   variant today.
4. Calls `wp_input_method_keyboard_grab_v2` for the active text-input
   client to receive raw keycodes.
5. For each keycode + modifier set, runs the active stack's six
   layers in-process and produces a sequence of events.
6. Emits `preedit_string` / `commit_string` events back to the
   compositor.

**Mode** — the daemon supports both axes of IM activation, but
crucially also takes the lower-layer choice itself:

- **Stack selection**: which layout recipe (Physical × … × IM) is
  active for the text input. Changed via the GUI tray, a global
  hotkey (Mod+Shift+Space by convention), or programmatically by a
  workspace switcher. Replaces the xkb-layout-switch path entirely.
- **IM toggle (within a stack)**: for stacks that ship multiple IMs
  (a Latin base with both pinyin and bopomofo, say), Mod+Space
  cycles. Stacks with one IM have nothing to cycle.

**Process model**: one daemon per user session, started by the
desktop session (`systemd --user` unit shipped with the package).
The GUI configurator (`flexkb gui`) talks to the daemon over a
Unix socket for live state inspection — preedit content, candidate
list, validation diagnostics, and the in-flight value of every
intermediate layer (the GUI's "show me the symbol stream between
Substitution and Compose" debug view becomes trivial because all
layers are local in-process).

**xkb relationship**: the static `flexkb build` pipeline still
exists and produces an xkb tree. That tree is for users who DON'T
run the daemon (X11 sessions, minimal Wayland compositors without
input-method-v2 support, embedded targets). The two paths share the
same `data/*.yaml` source so behaviour matches; the daemon is just
the more capable consumer that picks up the stateful IM tier the
static path can't express.

**X11**: out of scope for the initial implementation. Inherited via
the ibus backend (see "Phased backend plan" below) once that's
done — every X11 IM client supports ibus.

## Phased backend plan

The daemon's *transport* (how it talks to compositors and apps) is
separable from its *engine* (how it processes keystrokes through
the rule files). One engine, many backends, picked at runtime
based on what's available:

```
                  ┌─ Wayland input-method-v2 ──── KDE, Sway, Hyprland, river,
                  │                                labwc, niri, …
flexkb-imed ──────┤
                  ├─ ibus dbus (incoming) ──────── GNOME, X11 clients
                  │
                  └─ ibus engine spawner ──────── existing ibus-engine-* processes
                                                    routed through flexkb-imed
       │
       ▼
   shared engine: runtime resolver + IM tier + dictionaries
```

Implementation order, smallest viable scope first:

1. **Wayland v2 backend, native engines only.**
   Sufficient for KDE / wlroots users running flexkb's own
   data-driven IMs. The daemon binds zwp_input_method_v2 +
   keyboard_grab and runs every keystroke through the static
   resolver and the IM tier in-process. No external IM
   dependencies.

2. **ibus dbus backend.**
   flexkb-imed claims `org.freedesktop.IBus` on the session bus
   and implements the methods compositors and apps actually use
   (FocusIn, FocusOut, ProcessKeyEvent, Reset, SetCursorLocation,
   the input-context lifecycle). This lets the SAME daemon serve
   GNOME / Mutter (which only knows ibus) and X11 clients (also
   only ibus). No engine-process layer — flexkb-imed answers
   ibus dbus calls directly from its in-process engine.

3. **Host other IMEs.**
   The dominant CJK IMs (libpinyin, anthy, mozc, kkc, …) exist
   as standalone ibus-engine-* binaries with years of language
   modeling and dictionaries we won't replicate any time soon.
   flexkb-imed can spawn those binaries as child processes and
   route the ibus engine-side dbus traffic between them and the
   IM clients. Users get flexkb's modular layouts AND the
   existing engine ecosystem from the same daemon.

   Also via the same mechanism: other Wayland v2 IMEs (fcitx5,
   etc.) plug in by speaking ibus (most already do as a fallback)
   or — phase 3.1 — by connecting to a re-broadcast
   input-method-v2 interface flexkb-imed exposes for downstream
   IMEs.

Each phase is independently shippable: phase 1 gets a working
daemon on the modern Wayland stack; phase 2 doubles the
addressable user base to include GNOME and X11; phase 3 makes
flexkb-imed a drop-in replacement for ibus-daemon. The "remove
ibus from your system" promise lands at phase 2; "and host the
engines you used to need ibus for" lands at phase 3.

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
| `flexkb` | IME daemon + GUI configurator + ALL `data/*.yaml`. Runs the full layer stack in-process. Coexists peacefully with anything; libxkbcommon stays in the compositor for its own use but is not in the user's text-input path. | nothing |
| `flexkb-xkb` | Static xkb tree compiled from the same `data/*.yaml`. For users on X11, on Wayland compositors without input-method-v2, or who want xkeyboard-config replaced system-wide. Opt-in. | `xkeyboard-config` |
| `flexkb-data-cjk` | Dictionaries: CC-CEDICT, JMdict, … Split out for licensing and download size. | nothing |

Default install is `flexkb` alone — the daemon plus its data. The
existence of `flexkb-xkb` reflects that the static xkb path is a
*compatibility shim* for environments where the daemon isn't a fit,
not the project's reason for being. Same `data/*.yaml` under
`/usr/share/flexkb/data/` regardless of which package shipped it; the
daemon and the xkb compiler are two different consumers of one
source tree.

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

## Phase 3.1: Wayland v2 rebroadcast (implemented)

Status as of phase-3.1 implementation: the server side is in
`internal/wlserver/` + `internal/wlim/server.go`; the bridge that
wires it into ibus routing is `internal/imv2bridge`. Enabled with
`flexkb-imed --v2-rebroadcast` alongside `--ibus=alongside|replace`.
The socket lives at `$XDG_RUNTIME_DIR/flexkb-imed-v2.sock`;
downstream v2 IMEs set `WAYLAND_DISPLAY` to that path before
launching. Three-tier routing (v2 → ibushost engine → in-process
Session) lives in `internal/ibus/context.go`'s `ProcessKeyEvent`.

The original design sketch follows for reference; the implementation
matches it.

### Original sketch

flexkb-imed exposes its OWN `zwp_input_method_manager_v2` global
on a side socket. Downstream IMEs (v2-native tooling, or
hypothetical v2-only engines) connect to that socket and become
"input methods" within flexkb-imed's protocol view, just as
flexkb-imed itself is one inside a real wlroots compositor.

Use case that motivates this even on systems where the
compositor already supports v2 (KDE, wlroots): users on
Mutter/GNOME who explicitly want v2 semantics rather than ibus.
GNOME's stance ("ibus handles everything") foreclosed that path
at the compositor level; flexkb-imed-as-v2-server reopens it
for users who care.

Architecture once built:

```
                ┌── ibus client (Mutter / GTK / Qt app)
                │       │
                │       │ ProcessKeyEvent over dbus
                │       ▼
                │   flexkb-imed (ibus server, 5.2)
                │       │
                │       │ if a v2 IME has grabbed,
                │       │ forward keystroke as a v2
                │       │ key event over the side socket
                │       ▼
                │   v2 IME client (fcitx5 etc.)
                │       │ commit_string back over v2
                │       ▼
                │   flexkb-imed wraps as ibus CommitText
                │       │
                │       ▼
                └─── ibus client receives committed text
```

Implementation outline (the work, for the next session):

1. **`internal/wlserver/`** — server-side counterpart to
   `wlclient`. Listen on a Unix socket (separate from the
   compositor's), accept connections (one per downstream
   client), per-connection Dispatcher with the server-side
   object-id range (≥ 0xFF000000 per spec). The wire layer
   in `wlwire` is symmetric — encode/decode works in either
   direction unchanged.

2. **wl_display + wl_registry server implementations.** Same
   protocols, opposite side. Globals get *advertised* (not
   bound); bind requests get serviced. Sync requests get
   replied to.

3. **`internal/wlim/server.go`** — server side of
   `zwp_input_method_manager_v2` and the dependent types
   (input_method, keyboard_grab). Receive `get_input_method`,
   create input_method objects; service `grab_keyboard`,
   route incoming keystrokes (from the ibus-side bridge) as
   keyboard_grab key events; receive `commit_string` /
   `set_preedit_string` from the downstream IME and forward
   to the ibus input context that triggered the keystroke.

4. **Bridge in `internal/ibus.InputContext.ProcessKeyEvent`**
   — when a v2 client has the grab, route through them FIRST,
   then to internal/ibushost engines, then to the in-process
   Session. Three-tier fallback in the same order
   priority-wise as the current two-tier engine + Session.

5. **Socket configuration.** Side socket at
   `$XDG_RUNTIME_DIR/flexkb-imed-v2.sock`; downstream IMEs
   set `WAYLAND_DISPLAY` to that path before connecting. The
   GUI can offer a "copy WAYLAND_DISPLAY override to
   clipboard" affordance for users who want to start a v2
   IME against us.

Estimated effort: ~800 LOC for wlserver foundation,
~400 LOC for the v2 server-side interfaces, ~200 LOC bridge
wiring, ~400 LOC tests with socketpair-based downstream-
client fakes. Two focused sessions feels right.

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

1. **IM engine.** Schema parser, FSM evaluator, dictionary loader.
   Pure Go, no daemon. Compose-chain adapter proves the unification
   claim (one-state IM = X-Compose chain). Unit-tested with synthetic
   IMs and the existing devanagari compose chain.
2. **Static-layer runtime evaluators.** Today's `internal/compose/`
   produces xkb files at build time. Add an in-memory evaluator with
   the same composition rules that takes `(keycode, modifiers)` and
   returns a symbol — the API the daemon calls per keystroke.
   Reuses the existing data loaders, no new file format.
3. **Compositional validation.** Extend `tests/matrix_test.go` and
   add `flexkb validate`. Walks a stack's produced character set vs.
   the target locale's required set; reports gaps with actionable
   suggestions.
4. **m17n-mim import pipeline.** Convert a handful of mim files,
   exercise the engine on real-world IM data.
5. **Wayland daemon.** `input-method-v2` protocol binding plus
   keyboard-grab. Wires the layer evaluators (step 2) and the IM
   engine (step 1) into a session loop. Lives as `flexkb daemon` or
   a separate `flexkb-imed` binary.
6. **GUI integration.** Live preedit display, validation panel, the
   "show me every intermediate layer's output" debug view (trivial
   since all layers are in-process by step 5).
7. **Packaging split.** `flexkb` (daemon + data + GUI) as the default;
   `flexkb-xkb` (static-tree compatibility) opt-in;
   `flexkb-data-cjk` (dictionaries) separate for licensing.

Each step is independently usable: after step 1 you can `flexkb run-im
<name> <stream>` and see commits; after step 2 you can exercise the
full pipeline as a library; after step 3 you can audit combinations;
after step 5 you can actually type with it; after step 7 you can
ship.
