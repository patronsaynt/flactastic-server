#!/usr/bin/env bash
# Builds TagLib (the Mac app's exact pin) as static libraries into .deps/taglib.
# Prints nothing and exits early when the build is already in place.
set -Eeuo pipefail

VERSION="2.3.2"
SHA256="3ca2d8afaa7f1cf7f6ed10e511ebc368bfacd6dcaa3dbfa690b89e502e8963dc"
URL="https://github.com/taglib/taglib/releases/download/v$VERSION/taglib-$VERSION.tar.gz"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPS="$ROOT/.deps"
PREFIX="$DEPS/taglib"
STAMP="$PREFIX/.built-$VERSION-$(uname -m)"

[[ -f "$STAMP" ]] && exit 0
command -v cmake >/dev/null || { echo "cmake not found" >&2; exit 1; }

mkdir -p "$DEPS/download"
TARBALL="$DEPS/download/taglib-$VERSION.tar.gz"
[[ -f "$TARBALL" ]] || { curl -fsSL "$URL" -o "$TARBALL.part" && mv "$TARBALL.part" "$TARBALL"; }
if [[ "$( (sha256sum || shasum -a 256) <"$TARBALL" 2>/dev/null | cut -d' ' -f1)" != "$SHA256" ]]; then
    rm -f "$TARBALL"
    echo "taglib-$VERSION.tar.gz doesn't match its pinned checksum" >&2
    exit 1
fi

rm -rf "$DEPS/src" "$DEPS/build" "$PREFIX"
mkdir -p "$DEPS/src"
tar -xzf "$TARBALL" -C "$DEPS/src"
cmake -S "$DEPS/src/taglib-$VERSION" -B "$DEPS/build" \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_INSTALL_PREFIX="$PREFIX" \
    -DCMAKE_INSTALL_LIBDIR=lib \
    -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
    -DBUILD_SHARED_LIBS=OFF \
    -DBUILD_BINDINGS=ON \
    -DWITH_ZLIB=ON \
    -DBUILD_TESTING=OFF \
    -DBUILD_EXAMPLES=OFF >/dev/null
cmake --build "$DEPS/build" -j "$(getconf _NPROCESSORS_ONLN)" >/dev/null
cmake --install "$DEPS/build" >/dev/null
rm -rf "$DEPS/src" "$DEPS/build"
touch "$STAMP"
