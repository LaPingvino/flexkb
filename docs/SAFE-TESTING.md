# Safe testing & recovery

flexkb 0.2.0 is a substantial change: the package was split, a new
daemon was added, and the xkb-replacement path now lives in an
opt-in `flexkb-xkb` subpackage. This document covers the safe ways
to try the new build before committing your system to it, and the
exact commands to recover if something breaks.

## Test without installing

You don't need to install anything to verify the new code works.
Run the binaries directly from the source tree:

```sh
cd flexkb
go build ./cmd/flexkb
go build ./cmd/flexkb-imed

# Audit your current system state.
./flexkb doctor

# Dry-run the daemon: walks startup, probes the compositor and
# session bus, exits without grabbing the keyboard or claiming
# org.freedesktop.IBus. Safe on a live session — coexists with
# whatever IME you currently use.
./flexkb-imed --dry-run

# Try the daemon with no transports enabled (control-socket only).
# Useful to verify the data tree parses cleanly.
./flexkb-imed --wayland=false --ibus=off
```

The daemon's `--dry-run` is specifically designed for "I want to
know it WOULD work without committing to running it". It tells you
which backends would be available and what the configured stack
looks like.

## Test in a chroot

Before `makepkg -si` on your live system, build and install the
package in a clean Arch chroot. This catches any build-host-
specific issues before they touch /usr/share/X11/xkb on your real
machine:

```sh
# One-time setup (~/chroots is the conventional location).
mkdir -p ~/chroots
mkarchroot ~/chroots/root base-devel

# Build the package inside the chroot. The build runs with no
# access to your real system; if anything is missing or wrong,
# the chroot fails to install, not your machine.
cd flexkb
makechrootpkg -c -r ~/chroots
```

The output `.pkg.tar.zst` files appear in the current directory.
Inspect them with `pacman -Qpi flexkb-*.pkg.tar.zst` and only
install them on your real system once they look right.

## Recovery commands

### If `flexkb-xkb` breaks your keyboard

The xkb-replacement subpackage is the only thing that can actually
brick keyboard input. flexkb-xkb declares `provides=xkeyboard-config`,
which means reinstalling stock xkeyboard-config replaces it cleanly:

```sh
sudo pacman -S --overwrite '*' xkeyboard-config
```

Then log out and back in. Your keyboard returns to the upstream xkb
tree. The flexkb daemon (in the `flexkb` package, separate) is
unaffected and continues to work if you had it running.

### If `flexkb-imed` misbehaves

```sh
# Stop it for this session.
systemctl --user stop flexkb-imed

# Stop it from autostarting next login.
systemctl --user disable flexkb-imed

# Or just kill it manually.
killall flexkb-imed
```

If you ran `flexkb-imed --ibus=replace` and your IME apps stopped
getting input:

```sh
killall flexkb-imed 2>/dev/null
ibus-daemon -drx          # restart real ibus-daemon
```

GTK/Qt apps with `GTK_IM_MODULE=ibus` or `QT_IM_MODULE=ibus` pick
up the restored ibus-daemon immediately on next focus.

### If you can't tell what's installed

```sh
flexkb doctor              # prints ownership, what's running,
                           # what's reachable, and recovery commands
                           # tailored to your situation
```

## Minimal-adjustments development loop

If you find a bug during testing and want to fix-and-retry without
the full package-rebuild cycle:

```sh
# 1. Edit the offending Go file or YAML.
$EDITOR internal/imsession/session.go

# 2. Rebuild.
go build ./cmd/flexkb-imed

# 3. Run the local build (NOT the installed one).
./flexkb-imed --wayland=true

# 4. Test. If it works, optionally rebuild the package.
makepkg -si
```

The daemon's data search path includes `./data` when run from a
repo checkout, so YAML edits under `data/` are picked up by the
local build without needing to be copied to `/usr/share/flexkb/data`.

## What `flexkb-imed` will NOT do

For honesty about blast radius:

* The daemon does NOT modify `/etc/X11/xorg.conf.d/00-keyboard.conf`,
  `/etc/vconsole.conf`, `~/.xkb/`, or any pacman-owned file.
* The daemon does NOT enable itself in systemd. The `.service`
  unit ships but `WantedBy=graphical-session.target` only fires
  if you explicitly `systemctl --user enable flexkb-imed`.
* The daemon's xkb path uses an in-memory composed layout from
  `data/*.yaml`; it doesn't write any xkb file.
* The daemon's ibus mode `off` (the default) means it doesn't
  touch the session bus at all.
* `--ibus=alongside` only claims `org.freedesktop.IBus` if no
  other process owns it — if ibus-daemon is running, claiming
  fails and flexkb-imed exits without disrupting anything.

The only operation that can disrupt a running IME is
`--ibus=replace`, and only when an existing ibus-daemon is
running. That's documented explicitly in the flag's help text.
