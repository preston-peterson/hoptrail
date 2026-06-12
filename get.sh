#!/usr/bin/env bash
#
# hoptrail one-line installer — central or probe, no build tools needed.
#
#   Central:
#     curl -fsSL https://raw.githubusercontent.com/preston-peterson/hoptrail/main/get.sh | bash
#
#   Probe (the Probes panel in the central's UI generates this with the
#   values filled in):
#     curl -fsSL https://raw.githubusercontent.com/preston-peterson/hoptrail/main/get.sh | bash -s -- --probe --id site-east --central http://192.0.2.10:8080 --token <token>
#
# What it does:
#   1. Finds the latest GitHub release.
#   2. Downloads the prebuilt binary for this machine's architecture
#      and verifies it against the release's sha256 checksums.
#   3. Downloads the matching source tree (for install.sh, the systemd
#      units, and the sudoers rule — not to compile anything).
#   4. Runs install.sh, passing through any arguments — so every flag
#      install.sh understands works here too.
#
# Building from source remains a git clone + ./install.sh away; this
# script exists so nobody needs go/npm on a box that just runs hoptrail.

set -euo pipefail

REPO="preston-peterson/hoptrail"

say()  { printf '\033[1m[get.sh]\033[0m %s\n' "$*"; }
fail() { printf '\033[31m[get.sh] Error:\033[0m %s\n' "$*" >&2; exit 1; }

# --- preflight ---------------------------------------------------------

case "$(uname -s)" in
    Linux) ;;
    *) fail "hoptrail runs on Linux (got: $(uname -s))" ;;
esac

case "$(uname -m)" in
    x86_64)          ARCH="amd64" ;;
    aarch64|arm64)   ARCH="arm64" ;;
    *) fail "no prebuilt binary for $(uname -m) — build from source instead:
  git clone https://github.com/${REPO}.git && cd hoptrail && ./install.sh" ;;
esac

for tool in curl tar sha256sum; do
    command -v "$tool" >/dev/null 2>&1 && continue
    pkg="$tool"
    [ "$tool" = "sha256sum" ] && pkg="coreutils"
    fail "missing required tool: $tool — install it with your distro's package manager first, e.g.:
  sudo apt install $pkg      (Debian/Ubuntu)
  sudo dnf install $pkg      (Fedora/RHEL)"
done

# The prebuilt release binaries link against glibc (the SQLite layer is
# CGO). The floor is set by the release build host — at release time,
# verify with:  objdump -T hoptrail | grep -oE 'GLIBC_[0-9.]+' | sort -uV | tail -1
# and bump this if it moves.
MIN_GLIBC="2.34"

SOURCE_HINT="build from source instead (needs go/npm):
  git clone https://github.com/${REPO}.git && cd hoptrail && ./install.sh"

if ldd --version 2>&1 | grep -qi musl; then
    # Don't point musl users at the source build: it would compile,
    # but install.sh needs systemd, which musl distros (Alpine, etc.)
    # almost never run — they'd hit a confusing failure much later.
    fail "this system uses musl libc; the prebuilt binaries need glibc ${MIN_GLIBC}+.
Building from source would work, but hoptrail's installer also needs systemd,
which this system likely doesn't have — musl-based distros aren't supported yet."
fi
GLIBC_VER=$(getconf GNU_LIBC_VERSION 2>/dev/null | awk '{print $2}') || true
if [ -n "${GLIBC_VER:-}" ]; then
    if [ "$(printf '%s\n' "${MIN_GLIBC}" "${GLIBC_VER}" | sort -V | head -1)" != "${MIN_GLIBC}" ]; then
        fail "this system has glibc ${GLIBC_VER}, older than the ${MIN_GLIBC} the prebuilt binaries need — ${SOURCE_HINT}"
    fi
else
    say "warning: could not determine the glibc version — continuing. If hoptrail later fails to start with a GLIBC error, ${SOURCE_HINT}"
fi

# --- find the latest release -------------------------------------------

say "looking up the latest release of ${REPO}…"
# Fetch fully before parsing — grep -m1 on a live pipe makes curl die
# of a closed pipe under pipefail.
RELEASE_JSON=$(curl -fsSL -H 'Accept: application/vnd.github+json' \
        "https://api.github.com/repos/${REPO}/releases/latest") \
    || fail "could not reach the GitHub API"
TAG=$(printf '%s' "${RELEASE_JSON}" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name"[^"]*"([^"]+)".*/\1/' || true)
[ -n "${TAG}" ] || fail "could not determine the latest release tag"
VER="${TAG#v}"
say "latest release: ${TAG} (linux/${ARCH})"

# --- download into a scratch dir ---------------------------------------

WORKDIR=$(mktemp -d /tmp/hoptrail-get.XXXXXX)
trap 'rm -rf "${WORKDIR}"' EXIT
cd "${WORKDIR}"

DL="https://github.com/${REPO}/releases/download/${TAG}"
BIN_TAR="hoptrail_${VER}_linux_${ARCH}.tar.gz"
SUMS="hoptrail_${VER}_checksums.txt"

say "downloading ${BIN_TAR}…"
curl -fsSL -o "${BIN_TAR}" "${DL}/${BIN_TAR}" \
    || fail "download failed: ${DL}/${BIN_TAR}"
curl -fsSL -o "${SUMS}" "${DL}/${SUMS}" \
    || fail "download failed: ${DL}/${SUMS}"

say "verifying sha256…"
grep " ${BIN_TAR}\$" "${SUMS}" | sha256sum -c - >/dev/null \
    || fail "sha256 verification FAILED for ${BIN_TAR} — refusing to install"

say "downloading the ${TAG} source tree (installer + service units)…"
curl -fsSL -o source.tar.gz "https://github.com/${REPO}/archive/refs/tags/${TAG}.tar.gz" \
    || fail "download failed: source tree for ${TAG}"
tar -xzf source.tar.gz
SRC_DIR="${WORKDIR}/hoptrail-${VER}"
[ -d "${SRC_DIR}" ] || fail "unexpected source layout in ${TAG} tarball"

# The prebuilt binary at the source root is exactly what install.sh
# looks for before deciding it would have to build.
tar -xzf "${BIN_TAR}" -C "${SRC_DIR}"
[ -x "${SRC_DIR}/hoptrail" ] || fail "binary missing from ${BIN_TAR}"

# --- hand off to install.sh --------------------------------------------

say "running install.sh${*:+ $*}…"
cd "${SRC_DIR}"
if [ -t 0 ]; then
    ./install.sh "$@"
elif (exec < /dev/tty) 2>/dev/null; then
    # Piped through bash: reattach stdin so sudo's password prompt and
    # any interactive questions still reach the operator.
    ./install.sh "$@" < /dev/tty
else
    # No terminal at all (CI, automation): only works when every
    # required answer is supplied as flags.
    ./install.sh "$@" \
        || fail "install.sh needs a terminal for its prompts — download get.sh and run it directly, or pass all required flags"
fi
