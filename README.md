# flexkb

**Modular XKB layout generator.** A drop-in replacement for `xkeyboard-config`
that decomposes keyboard layouts into independent dimensions instead of one
giant flat file tree:

```
   Physical  ×  Transformation  ×  Additions  ×  Substitutions  ×  Compose
   (ANSI,        (QWERTY,           (intl,         (Latin↔Cyrillic    (devanagari
    ISO,          Dvorak,            polish,        phonetic,          conjuncts,
    JIS, …)       Colemak,           german,        Latin↔Greek,       latin
                  AZERTY,            esperanto,     Latin↔Hebrew,      ligatures,
                  BÉPO,              spanish, …)    Latin↔Arabic, …)   symbol
                  Workman,                                             triples,
                  Norman,                                              …)
                  ЙЦУКЕН,
                  Dubeolsik,
                  Kedmanee, …)
```

The core promise: **support less to provide more.** Adding one new
transformation automatically unlocks it on every physical layout and
across every existing addition. One Russian-phonetic substitution gives
you phonetic Russian on QWERTY, Dvorak, Colemak, Workman, Norman — no
hand-written variants per base.

**AZERTY isn't broken because of physics; it's broken because xkb confused
*physical hardware*, *letter arrangement*, and *language* into one bundled
"layout name". flexkb separates them.**

## Status

51 modular layout files, **173 variants**, **14 scripts with phonetic
substitutions**, **13 native transformations** (Latin + non-Latin).
Drop-in compatibility enforced at build time — the generated tree is a
strict superset of the upstream xkeyboard-config tree.

Scripts with native + phonetic coverage: Latin (QWERTY/QWERTZ/AZERTY/
Dvorak/Colemak/Workman/Norman/BÉPO/Turkish-Q/F), Cyrillic (ЙЦУКЕН +
phonetic), Greek, Hebrew, Arabic, Armenian, Georgian, Devanagari,
Bengali, Tamil, Sinhala, Khmer, Lao, Thai (Kedmanee + phonetic),
Korean (Dubeolsik), Berber/Tifinagh (phonetic). Plus regional Cyrillic
dialects (Ukrainian, Belarusian, Bulgarian, Macedonian, Serbian,
Mongolian, Kazakh) and Arabic dialects (Persian, Urdu) via small
extension additions.

`go test -v -run TestCoverage ./tests` prints a per-variant report card
against the system xkeyboard-config tree.

## The four modular dimensions

### Physical
Which key codes exist on the hardware. ANSI, ISO, JIS. No symbol info,
just a list of `<TLDE>`, `<AE01>`, `<AC01>`, etc.

### Transformation
Per-key level-1/level-2 base symbols. **Latin transformations**: QWERTY,
QWERTZ, AZERTY, Dvorak, Colemak, Workman, Norman, BÉPO, Turkish-Q,
Turkish-F. **Native non-Latin**: ЙЦУКЕН (Russian), Hebrew, Arabic-101,
Dubeolsik (Korean), Kedmanee (Thai). Tagged with a `script:` field so
the matrix sanity test only checks compatible pairs.

### Additions
Decorations layered on top. Three flavours:

- **Position overlays**: glyph on specific physical key (German umlauts
  on AC10/AC11/AD11; traditional Portuguese ç on AC10).
- **Letter-following overlays**: glyph tracks the underlying letter
  wherever it lives. ONE `intl` addition replaces N hand-written
  transformation-specific variants — works on QWERTY, Dvorak, Colemak,
  Workman, Norman, Turkish bases automatically.
- **Fallback + priority**: letter overlays can list fallback level-1
  tokens and a priority (`high` / `""` / `low`). Higher priority wins
  key claims; lower yields silently. Example: `polish-on-intl` claims
  L→ł over `intl`'s normal-priority L→oslash.

### Substitution
Character-level rewrite as a final pass. The mechanism behind every
phonetic non-Latin layout. Apply Latin transformation, then substitute
each Latin letter for its target-script cognate. `~name` for inverse
direction. Chain multiple — hotfix narrow overrides FIRST in the chain
pre-empt a broader map (`hotfix-russian-w-as-zhe` before
`latin-cyrillic-phonetic`).

### Compose
Post-keypress sequences that produce composed glyphs xkb can't bind to
a single level: Devanagari conjuncts (क्ष = क + virama + ष), Latin
ligatures, currency triples. Each `data/compose/<name>.yaml` declares
structured `input → [codepoints]` bindings; the build emits an X11
Compose file (`<out>/Compose.d/<layout>-<variant>`) and `flexkb
activate` installs the current variant's chain to `~/.XCompose`. The
YAML is the source of truth — a future native runtime can read it
directly without going through xkb or libX11.

## Repo layout

```
data/
  physical/        # which keys exist (ansi.yaml, iso.yaml, jis.yaml)
  transformations/ # base level 1/2 symbol arrangement (qwerty, dvorak,
                   # ycuken, kedmanee, dubeolsik, …)
  additions/       # level 3/4 overlays + dead keys
                   # (intl, polish, esperanto, persian-extras, …)
  substitutions/   # character-level rewrites
                   # (latin-cyrillic-phonetic, latin-greek-phonetic, …)
  compose/         # post-keypress composed-glyph sequences
                   # (latin-devanagari-phonetic conjuncts, …)
  layouts/         # recipes binding it all together into named variants

internal/
  model/           # data structs + YAML loading + XDG discovery
  compose/         # physical + transformation + additions + substitutions
  xkbwriter/       # emit xkb_symbols { … } blocks
  xkbparser/       # parse xkb_symbols files (+ fuzz target)
  rulespatch/      # patch evdev.xml/.lst so new variants surface in pickers
  dropincheck/     # enforce strict-superset of upstream at build time
  bulkconvert/     # verbatim-copy fallback for unmodularised files

cmd/flexkb/        # CLI binary
docs/DESIGN.md     # architecture deep-dive
tests/             # coverage report, matrix sanity test
```

## CLI

```sh
flexkb list                                    # list known modular variants
flexkb compose us basic                        # print one variant to stdout
flexkb compose --xcompose in hindi-phonetic    # print variant's X-Compose chain
flexkb generate ./out                          # write all modular layouts
flexkb build ./out --xkb /usr/share/X11/xkb    # generate + fallback-copy rest
flexkb verify us basic                         # diff composed vs system xkb file
flexkb dropin-check ./out /usr/share/X11/xkb   # fail if not strict superset
flexkb paths                                   # show data dirs being consulted
flexkb info --xkb /path us                     # per-layout modular/passthrough report
flexkb migrate-suggest us dvorak-intl          # heuristic mapping of old name
flexkb activate pt dvorak                      # generate to ~/.xkb + setxkbmap
                                               # + ~/.XCompose if compose: set
```

## Per-user customisation

`flexkb` walks an ordered data search path, highest priority first:

1. `$XDG_CONFIG_HOME/flexkb/data` (default `~/.config/flexkb/data`)
2. `$XDG_DATA_HOME/flexkb/data`
3. `./data` (when running from a repo)
4. `/usr/share/flexkb/data` (system install)

Same-name files in a higher-priority directory shadow lower ones; new
files just add. So you can:

- Override `data/transformations/dvorak.yaml` to taste — pacman upgrades
  never touch your copy.
- Drop a brand-new `data/layouts/my-mix.yaml` declaring any combination
  of the existing physical / transformation / additions / substitutions
  — it appears in `flexkb list` and composes immediately.
- Activate any variant for the current X session:
  `flexkb activate my-mix mine` writes to `~/.xkb/` and runs xkbcomp.

## Designing layouts

A Portuguese Dvorak user just composes existing parts:

```yaml
# data/layouts/pt.yaml
- name: dvorak
  description: Portuguese (Dvorak base, ç on AltGr+c)
  physical: iso
  transformation: dvorak
  additions: [portuguese]
```

A Russian-phonetic-on-Colemak user too:

```yaml
- name: phonetic-colemak
  physical: ansi
  transformation: colemak
  substitutions: [latin-cyrillic-phonetic]
  additions: [russian-phonetic-extras]
```

Stack for languages with hand-picked priorities:

```yaml
# Polish using intl + Polish-priority overrides
- name: intl
  physical: iso
  transformation: qwerty
  additions: [intl, polish-on-intl]   # polish-on-intl claims L→ł over intl
```

## Building the package (Arch)

```sh
makepkg -si
```

Builds `flexkb`, runs it against `/usr/share/xkeyboard-config-2`,
verifies the result is a strict superset via `flexkb dropin-check`, then
installs everything to `/usr/share/X11/xkb`. Conflicts with and provides
`xkeyboard-config` so dependent packages stay satisfied. Includes an
informational `.install` hook that snapshots the user's pre-install
keyboard config and prints a banner; correctness lives in the package
contents, not the hook.

## Architecture

See [docs/DESIGN.md](docs/DESIGN.md) for the formal composition rules,
priority semantics, and drop-in mechanism.

## License

MIT — see [LICENSE](LICENSE).
