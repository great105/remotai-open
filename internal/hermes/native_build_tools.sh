#!/usr/bin/env bash
# Build the upstream locked cryptography release on Intel macOS, which no
# longer has an upstream wheel. All SDK files stay inside Remotai's toolchain.
set -euo pipefail
toolchain="$1"
case "$toolchain" in /*) ;; *) echo 'The owned toolchain must be an absolute path' >&2; exit 1 ;; esac
for command_name in curl shasum clang make perl tar; do
    command -v "$command_name" >/dev/null || { echo "Missing build prerequisite: $command_name" >&2; exit 1; }
done
clang --version >/dev/null
mkdir -p "$toolchain/native-build"
export CARGO_HOME="$toolchain/cargo"
export RUSTUP_HOME="$toolchain/rustup"
export PATH="$CARGO_HOME/bin:$PATH"
export RUSTUP_TOOLCHAIN='1.99.0'
export OPENSSL_DIR="$toolchain/openssl-3.5.9"
export OPENSSL_STATIC=1

fetch_verified() {
    local url="$1" expected="$2" destination="$3" actual
    curl --fail --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 300 --retry 2 "$url" -o "$destination.part"
    actual="$(shasum -a 256 "$destination.part")"
    [ "${actual%% *}" = "$expected" ] || { echo 'Compiler download checksum mismatch' >&2; exit 1; }
    mv "$destination.part" "$destination"
}

if ! "$CARGO_HOME/bin/rustc" --version >/dev/null 2>&1; then
    fetch_verified 'https://static.rust-lang.org/rustup/archive/1.28.2/x86_64-apple-darwin/rustup-init' \
        '9c331076f62b4d0edeae63d9d1c9442d5fe39b37b05025ec8d41c5ed35486496' "$toolchain/native-build/rustup-init"
    chmod 700 "$toolchain/native-build/rustup-init"
    RUSTUP_INIT_SKIP_PATH_CHECK=yes "$toolchain/native-build/rustup-init" -y --no-modify-path --profile minimal --default-toolchain 1.99.0
fi
"$CARGO_HOME/bin/rustc" --version

if [ ! -f "$OPENSSL_DIR/lib/libcrypto.a" ]; then
    archive="$toolchain/native-build/openssl-3.5.9.tar.gz"
    fetch_verified 'https://github.com/openssl/openssl/releases/download/openssl-3.5.9/openssl-3.5.9.tar.gz' \
        '603f5602e2eef00d77fbd429d34dcd5822bb301757a1bc9cdb24c670f1eb859a' "$archive"
    build_dir="$(mktemp -d "$toolchain/native-build/openssl.XXXXXX")"
    tar -xzf "$archive" -C "$build_dir"
    (
        cd "$build_dir/openssl-3.5.9"
        perl Configure darwin64-x86_64-cc no-shared no-tests --prefix="$OPENSSL_DIR" --openssldir="$OPENSSL_DIR/ssl" --libdir=lib
        make -j2
        make install_sw
    )
fi
test -f "$OPENSSL_DIR/include/openssl/ssl.h"
test -f "$OPENSSL_DIR/lib/libcrypto.a"
"$CARGO_HOME/bin/cargo" --version
echo 'Owned Intel macOS compiler prerequisites are ready'
