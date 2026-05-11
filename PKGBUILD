# Maintainer: Joop Kiefte <ikojba@gmail.com>
#
# flexkb — modular XKB layout generator. Replaces xkeyboard-config by
# generating an XKB tree composed of modular Physical × Transformation ×
# Additions sources, falling back to verbatim-copied xkeyboard-config files
# for anything not yet modularised. The PKGBUILD therefore needs
# xkeyboard-config available at build time (as the fallback source) but
# replaces it at install time so userspace ends up with one canonical
# /usr/share/X11/xkb tree.

pkgname=flexkb
pkgver=0.1.0
pkgrel=1
pkgdesc="Modular XKB layout generator and drop-in xkeyboard-config replacement"
# CGO is enabled to link the webview engine, so the package is no
# longer arch-independent.
arch=('x86_64' 'aarch64')
url="https://github.com/lapingvino/flexkb"
license=('MIT')
# Hard deps: webkit2gtk-4.1 is loaded by the `flexkb gui` webview engine
# at runtime; the binary will fail to start without it. xdg-utils for the
# browser-fallback engine (xdg-open).
depends=('webkit2gtk-4.1' 'xdg-utils')
# Optional: a Chromium-based browser unlocks the lorca engine
# (`flexkb gui --engine=lorca`, a chromeless --app= window).
optdepends=('chromium: enables `flexkb gui --engine=lorca` (chromeless app window)'
            'google-chrome: enables `flexkb gui --engine=lorca`'
            'microsoft-edge-stable-bin: enables `flexkb gui --engine=lorca`')
# We provide xkeyboard-config so any package depending on it stays satisfied
# (libxkbcommon, xorg-server, gnome-control-center, etc.).
provides=("xkeyboard-config=${pkgver}.upstream-2.47")
conflicts=('xkeyboard-config')
# Build deps: go for the binary, gcc/pkgconf for CGO+webkit headers,
# xkeyboard-config for the fallback xkb tree we mirror anything-not-
# yet-modularised from.
makedepends=('go' 'gcc' 'pkgconf' 'xkeyboard-config')
# When packaging from a checkout, set source to () and just run makepkg in
# place; when releasing, point source at a git tag tarball.
source=()
sha256sums=()
install=flexkb.install

build() {
    cd "$startdir"
    # CGO_ENABLED=1 is required by the webview engine (webkit2gtk-4.1).
    # The lorca engine has no link-time deps (just runs a system chromium
    # at runtime) so it tags in cleanly alongside.
    export CGO_ENABLED=1
    export GOFLAGS="-trimpath -mod=readonly -modcacherw"
    export GOCACHE="$srcdir/.gocache"
    go build -tags 'webview lorca' \
        -ldflags "-s -w -X main.version=$pkgver" -o flexkb ./cmd/flexkb

    # Run our build pipeline: generate modular xkb files into pkg-staging/,
    # then copy everything else verbatim from the installed xkeyboard-config.
    rm -rf "$srcdir/staging"
    ./flexkb build --xkb /usr/share/xkeyboard-config-2 "$srcdir/staging"
}

check() {
    cd "$startdir"
    # Avoid `./...` because it would walk fakeroot's pkg/ tree which makepkg
    # creates with root-owned permissions partway through.
    go test ./cmd/... ./internal/... ./tests/...

    # The drop-in promise: the built tree must be a strict superset of the
    # upstream xkeyboard-config tree we replace. Anything missing fails the
    # package build, so a regression in the generator can't silently ship.
    ./flexkb dropin-check "$srcdir/staging" /usr/share/xkeyboard-config-2
}

package() {
    cd "$startdir"

    # Install the binary.
    install -Dm755 flexkb "$pkgdir/usr/bin/flexkb"

    # Install the modular source data so users can rebuild and tweak from
    # the installed copy without needing a git checkout.
    install -d "$pkgdir/usr/share/flexkb"
    cp -r data "$pkgdir/usr/share/flexkb/data"

    # Install the generated xkb tree. The upstream layout puts files in
    # /usr/share/xkeyboard-config-2 with a /usr/share/X11/xkb symlink; we
    # take the simpler route and install directly under /usr/share/X11/xkb.
    install -d "$pkgdir/usr/share/X11"
    cp -r "$srcdir/staging" "$pkgdir/usr/share/X11/xkb"

    # Backwards compatibility: re-create the /usr/share/xkeyboard-config-2
    # symlink some tools still look at directly.
    install -d "$pkgdir/usr/share"
    ln -sf X11/xkb "$pkgdir/usr/share/xkeyboard-config-2"

    # Settings/control-panel launcher for the GUI preview. Uses the
    # standard freedesktop "preferences-desktop-keyboard" icon so it
    # picks up the user's icon theme without us shipping a custom one.
    install -Dm644 packaging/flexkb.desktop "$pkgdir/usr/share/applications/flexkb.desktop"

    install -Dm644 LICENSE "$pkgdir/usr/share/licenses/$pkgname/LICENSE"
    install -Dm644 README.md "$pkgdir/usr/share/doc/$pkgname/README.md"
}

# vim:set ts=4 sw=4 et:
