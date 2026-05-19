# Maintainer: Joop Kiefte <ikojba@gmail.com>
#
# flexkb — modular XKB layout generator + Wayland input-method-v2
# daemon + ibus dbus daemon, all driven from one modular data tree.
#
# Split into TWO packages:
#
#   flexkb       — daemons + GUI + the modular data tree under
#                  /usr/share/flexkb/data. Coexists peacefully with
#                  anything; does NOT conflict with xkeyboard-config.
#                  This is the default install: it gives users the
#                  IME tier (`flexkb-imed --wayland=true` on
#                  wlroots-protocols compositors or `--ibus replace`
#                  on GNOME/X11) plus the GUI configurator.
#
#   flexkb-xkb   — static xkb tree generated from flexkb's data
#                  tree at build time. Drop-in replacement for
#                  xkeyboard-config; conflicts with it. Install
#                  this when you want flexkb's modular layouts
#                  surfaced through the system xkb path (X11,
#                  Wayland compositors without input-method-v2
#                  support, etc.).
#
# Both pull data from /usr/share/flexkb/data so behaviour is
# identical regardless of which is installed; the xkb package is
# a *compatibility shim* over the same source tree, not a
# different layout authoring path.

pkgbase=flexkb
pkgname=('flexkb' 'flexkb-xkb')
pkgver=0.2.0
pkgrel=1
_xkbcver=2.47
pkgdesc="Modular XKB layout generator + Wayland IME + ibus daemon"
arch=('x86_64' 'aarch64')
url="https://github.com/lapingvino/flexkb"
license=('MIT')
# CGO is enabled to link the webview engine, so the package is no
# longer arch-independent.
makedepends=('go' 'gcc' 'pkgconf' 'meson' 'ninja' 'python' 'libxslt' 'gettext' 'xorg-xkbcomp')
# Pin the upstream xkeyboard-config we mirror anything-not-yet-
# modularised from. The flexkb-xkb subpackage builds against this;
# the flexkb subpackage doesn't need it.
source=("https://www.x.org/archive/individual/data/xkeyboard-config/xkeyboard-config-${_xkbcver}.tar.xz")
sha256sums=('SKIP')

build() {
    # Step 1 — build xkeyboard-config from its tarball so the .part
    # source files are assembled into actual rules/evdev,
    # rules/evdev.xml etc.
    cd "$srcdir/xkeyboard-config-${_xkbcver}"
    arch-meson . _build
    meson compile -C _build
    DESTDIR="$srcdir/xkbc-install" meson install -C _build
    _xkbc_tree="$srcdir/xkbc-install/usr/share/xkeyboard-config-2"
    if [[ ! -d "$_xkbc_tree" || -L "$_xkbc_tree" ]]; then
        _xkbc_tree="$srcdir/xkbc-install/usr/share/X11/xkb"
    fi
    if [[ -L "$_xkbc_tree" ]]; then
        _xkbc_tree="$(readlink -f "$_xkbc_tree")"
    fi
    [[ -f "$_xkbc_tree/rules/evdev" ]] || \
        { echo "fatal: xkeyboard-config build didn't produce rules/evdev at $_xkbc_tree"; exit 1; }

    # Step 2 — build the flexkb binaries.
    cd "$startdir"
    export CGO_ENABLED=1
    export GOFLAGS="-trimpath -mod=readonly -modcacherw"
    export GOCACHE="$srcdir/.gocache"
    # flexkb: CLI + GUI server (webview/lorca tags). The webview
    # build tag pulls in webkit2gtk-4.1; lorca needs only a system
    # chromium at runtime.
    go build -tags 'webview lorca' \
        -ldflags "-s -w -X main.version=$pkgver" -o flexkb ./cmd/flexkb
    # flexkb-imed: the IME daemon. No webview deps — pure protocol
    # binary, smaller and easier to run as a systemd --user service.
    go build \
        -ldflags "-s -w -X main.version=$pkgver" -o flexkb-imed ./cmd/flexkb-imed

    # Step 3 — generate the static xkb tree for the flexkb-xkb
    # subpackage. Uses the freshly-built xkeyboard-config from
    # step 1 as the upstream-fallback source.
    rm -rf "$srcdir/staging"
    ./flexkb build --xkb "$_xkbc_tree" "$srcdir/staging"
    echo "$_xkbc_tree" > "$srcdir/.xkbc_tree_path"
}

check() {
    cd "$startdir"
    go test ./cmd/... ./internal/... ./tests/...

    # The drop-in promise: the built tree must be a strict superset
    # of the upstream xkeyboard-config tree. Anything missing fails
    # the package build.
    local _xkbc_tree
    _xkbc_tree="$(cat "$srcdir/.xkbc_tree_path")"
    ./flexkb dropin-check "$srcdir/staging" "$_xkbc_tree"
}

package_flexkb() {
    pkgdesc="Modular XKB-and-beyond input stack: Wayland IME daemon, ibus daemon, GUI configurator"
    # Hard runtime deps. webkit2gtk for the webview GUI; xdg-utils
    # for the xdg-open browser fallback; dbus for the ibus backend.
    depends=('webkit2gtk-4.1' 'xdg-utils' 'dbus')
    optdepends=(
        'chromium: enables `flexkb gui --engine=lorca` (chromeless app window)'
        'google-chrome: enables `flexkb gui --engine=lorca`'
        'microsoft-edge-stable-bin: enables `flexkb gui --engine=lorca`'
        'flexkb-xkb: install if you also want flexkb to replace xkeyboard-config'
        'm17n-db: provides 200+ rule files the importer can read'
    )

    cd "$startdir"

    # Daemons & CLI.
    install -Dm755 flexkb "$pkgdir/usr/bin/flexkb"
    install -Dm755 flexkb-imed "$pkgdir/usr/bin/flexkb-imed"

    # The modular source data — read-only, the source of truth
    # every flexkb consumer (CLI, GUI, daemon, xkb subpackage) reads.
    install -d "$pkgdir/usr/share/flexkb"
    cp -r data "$pkgdir/usr/share/flexkb/data"

    # systemd --user unit so the daemon starts with the user session
    # (and gets restarted cleanly on logout/login).
    install -Dm644 packaging/flexkb-imed.service "$pkgdir/usr/lib/systemd/user/flexkb-imed.service"

    # GUI desktop entry.
    install -Dm644 packaging/flexkb.desktop "$pkgdir/usr/share/applications/flexkb.desktop"

    # Licensing + docs.
    install -Dm644 LICENSE "$pkgdir/usr/share/licenses/flexkb/LICENSE"
    install -Dm644 README.md "$pkgdir/usr/share/doc/flexkb/README.md"
    install -Dm644 docs/DESIGN.md "$pkgdir/usr/share/doc/flexkb/DESIGN.md"
    install -Dm644 docs/DESIGN-IME.md "$pkgdir/usr/share/doc/flexkb/DESIGN-IME.md"
}

package_flexkb-xkb() {
    pkgdesc="Drop-in xkeyboard-config replacement built from flexkb's modular data tree"
    depends=('flexkb')
    # Drop-in replacement: provide and conflict with xkeyboard-config
    # so any package depending on it stays satisfied.
    provides=("xkeyboard-config=${_xkbcver}")
    conflicts=('xkeyboard-config')
    install=flexkb-xkb.install

    # Install the generated xkb tree.
    install -d "$pkgdir/usr/share/X11"
    cp -r "$srcdir/staging" "$pkgdir/usr/share/X11/xkb"

    # Backwards-compat /usr/share/xkeyboard-config-2 symlink.
    install -d "$pkgdir/usr/share"
    ln -sf X11/xkb "$pkgdir/usr/share/xkeyboard-config-2"
}

# vim:set ts=4 sw=4 et:
