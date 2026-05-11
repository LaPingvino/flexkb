# flexkb

**Modular XKB layout generator.** A drop-in replacement for `xkeyboard-config`
that decomposes keyboard layouts into three independent dimensions instead of
one giant flat file tree:

```
       Physical  ×  Transformation  ×  Additions  =  N × M × K variants
       (ANSI,        (QWERTY,           (intl,         from N + M + K
        ISO,          QWERTZ,            polish,       source files
        JIS, …)       AZERTY,            german, …)
                      Dvorak,
                      Colemak,
                      Workman, …)
```

The core promise: **support less to provide more.** Adding one new
transformation (say, Neo) automatically unlocks Neo on every physical layout
and with every addition flexkb knows about. Adding one new addition (say,
Esperanto suprasignoj) unlocks it across every existing transformation.

## Status

This is a **proof of concept**. We ship a complete XKB tree by:

1. Generating modular variants for layouts flexkb knows about (currently
   `us`, `de`, `pl` with QWERTY/QWERTZ/Dvorak/Colemak/Workman crossings).
2. Falling back to verbatim-copied files from `xkeyboard-config` for every
   layout we haven't modularised yet — so the install is always complete.

Run the coverage report to see where we stand:

```sh
go test -v -run TestCoverage ./tests
```

It prints how many `xkb_symbols` files we own modularly, how many fall back
to copies, and per-variant byte-equivalence vs the system reference.

## Layout of the repo

```
data/
  physical/        # which keys exist (ansi.yaml, iso.yaml, jis.yaml)
  transformations/ # base level 1/2 symbol arrangement (qwerty, dvorak, …)
  additions/       # level 3/4 overlays + dead keys (intl, polish, …)
  layouts/         # recipes binding the three together into named variants

internal/
  model/           # data structs + YAML loading
  compose/         # physical + transformation + additions → final symbol map
  xkbwriter/       # emit xkb_symbols { … } blocks
  xkbparser/       # parse existing /usr/share/X11/xkb/symbols/* for compare
  bulkconvert/     # verbatim-copy fallback for not-yet-modularised layouts

cmd/flexkb/        # CLI binary
tests/             # integration tests (coverage report, round-trip checks)
```

## CLI

```sh
flexkb list                                # list known modular variants
flexkb compose us basic                    # print one variant to stdout
flexkb generate ./out                      # write all modular layouts to ./out/symbols/
flexkb build ./out --xkb /usr/share/X11/xkb  # generate + fallback-copy rest
flexkb verify us basic                     # diff composed vs system xkb file
```

## Designing a new layout

A user who wants Polish Dvorak just composes existing parts — no new XKB
file required:

```yaml
# data/layouts/pl.yaml
- name: dvorak
  physical: iso
  transformation: dvorak
  additions: [polish]
```

That `pl(dvorak)` variant now exists. The `polish` addition still places
AltGr+a → ą at AC01, etc., regardless of whether Dvorak put 'a' there
(it did — Dvorak keeps 'a' on AC01).

## Building the package (Arch)

```sh
makepkg -si
```

This builds `flexkb`, runs it against `/usr/share/xkeyboard-config-2` to
produce a full XKB tree, then installs everything to `/usr/share/X11/xkb`,
conflicting with `xkeyboard-config` and providing it so dependent packages
stay satisfied.

## License

MIT — see [LICENSE](LICENSE).
