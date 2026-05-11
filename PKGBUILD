# Maintainer: Joop Kiefte <ikojba@gmail.com>
#
# flexkb — modular XKB layout generator. Replaces xkeyboard-config by
# generating an XKB tree composed of modular Physical × Transformation ×
# Additions sources, falling back to verbatim-copied xkeyboard-config
# files for anything not yet modularised.
#
# Build reads the verbatim-fallback content from a *pinned upstream
# xkeyboard-config tarball* fetched at build time — never from the
# live system. This avoids the self-amputating circular dependency
# we had when reading /usr/share/xkeyboard-config-2 directly: once
# flexkb is installed, that path resolves to flexkb's own tree, so
# any subsequent rebuild copied the previously-stripped flexkb tree
# instead of the real upstream, dropping rules/compat/keycodes/types
# files. Pinning to an x.org tarball makes the build reproducible
# regardless of the host system's xkb state.

pkgname=flexkb
pkgver=0.1.0
pkgrel=2
_xkbcver=2.47
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
provides=("xkeyboard-config=${_xkbcver}")
conflicts=('xkeyboard-config')
# Build deps: go for the binary, gcc/pkgconf for CGO+webkit headers.
# Crucially NOT xkeyboard-config — we fetch a pinned tarball below
# instead of reading the live system.
makedepends=('go' 'gcc' 'pkgconf')
# Pin the upstream xkeyboard-config we mirror anything-not-yet-
# modularised from. Tarball lives in $srcdir/xkeyboard-config-X.Y/
# after extraction.
source=("https://www.x.org/archive/individual/data/xkeyboard-config/xkeyboard-config-${_xkbcver}.tar.xz")
# Skipping the hash trusts x.org over HTTPS — pin a real hash here
# if you want byte-exact reproducibility / supply-chain verification.
sha256sums=('SKIP')
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
    # then copy everything else verbatim from the *pinned* upstream
    # xkeyboard-config tarball. Reading from $srcdir/xkeyboard-config-X.Y
    # rather than the live system avoids the self-amputation loop
    # described at the top of this file.
    rm -rf "$srcdir/staging"
    ./flexkb build --xkb "$srcdir/xkeyboard-config-${_xkbcver}" "$srcdir/staging"
}

check() {
    cd "$startdir"
    # Avoid `./...` because it would walk fakeroot's pkg/ tree which makepkg
    # creates with root-owned permissions partway through.
    go test ./cmd/... ./internal/... ./tests/...

    # The drop-in promise: the built tree must be a strict superset of the
    # upstream xkeyboard-config tree we replace. Anything missing fails the
    # package build, so a regression in the generator can't silently ship.
    # Reference is the pinned tarball, not the live system.
    ./flexkb dropin-check "$srcdir/staging" "$srcdir/xkeyboard-config-${_xkbcver}"
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
