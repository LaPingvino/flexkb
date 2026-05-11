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
arch=('any')
url="https://github.com/lapingvino/flexkb"
license=('MIT')
depends=()
# We provide xkeyboard-config so any package depending on it stays satisfied
# (libxkbcommon, xorg-server, gnome-control-center, etc.).
provides=("xkeyboard-config=${pkgver}.upstream-2.47")
conflicts=('xkeyboard-config')
# Build deps: go for the binary, xkeyboard-config for the fallback xkb tree
# we'll mirror anything we haven't modularised yet from.
makedepends=('go' 'xkeyboard-config')
# When packaging from a checkout, set source to () and just run makepkg in
# place; when releasing, point source at a git tag tarball.
source=()
sha256sums=()
install=flexkb.install

build() {
    cd "$startdir"
    export CGO_ENABLED=0
    export GOFLAGS="-trimpath -mod=readonly -modcacherw"
    export GOCACHE="$srcdir/.gocache"
    go build -ldflags "-s -w -X main.version=$pkgver" -o flexkb ./cmd/flexkb

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
