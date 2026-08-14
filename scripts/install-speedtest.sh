#!/bin/bash
# =============================================================================
# Hoptrail — speedtest CLI install helper
# =============================================================================
#
# Installs the Ookla speedtest CLI (the bandwidth-monitoring
# dependency) using the distro's package manager. Runs as ROOT:
# install.sh copies this file to /usr/local/lib/hoptrail/ (root-owned,
# so the service user can't swap it) and whitelists that exact path in
# /etc/sudoers.d/hoptrail — which is how the web UI's "install the
# speedtest CLI" button works without a terminal. install.sh's
# --add-bandwidth mode runs the same file via sudo.
#
# Output is plain text (no color codes): the web UI shows it verbatim
# when an install fails.
#
# Exit codes: 0 installed/already present · 1 install failed ·
#             3 unsupported distro (manual install required)
# =============================================================================

if [ "$(id -u)" -ne 0 ]; then
    echo "error: must run as root (via sudo)" >&2
    exit 1
fi

if command -v speedtest >/dev/null 2>&1; then
    echo "speedtest CLI already installed: $(speedtest --version 2>/dev/null | head -1)"
    exit 0
fi

if ! command -v curl >/dev/null 2>&1; then
    echo "error: curl is required to add the Ookla package repository" >&2
    exit 1
fi

# Distro family detection — same ID-then-ID_LIKE logic as install.sh.
detect_pkg_family() {
    local id="" like=""
    if [ -r /etc/os-release ]; then
        eval "$(. /etc/os-release; printf 'id=%q\nlike=%q\n' "${ID:-}" "${ID_LIKE:-}")"
    fi
    case "$id" in
        debian|ubuntu|raspbian|linuxmint|pop|elementary|neon|kali|parrot) echo debian; return ;;
        fedora|rhel|centos|rocky|almalinux|amzn|ol) echo fedora; return ;;
        arch|manjaro|endeavouros|garuda|artix|cachyos) echo arch; return ;;
        opensuse*|sles|sled) echo opensuse; return ;;
    esac
    case " $like " in
        *" debian "*|*" ubuntu "*) echo debian ;;
        *" fedora "*|*" rhel "*|*" centos "*) echo fedora ;;
        *" arch "*) echo arch ;;
        *" suse "*|*" opensuse "*) echo opensuse ;;
        *) echo "" ;;
    esac
}

family="$(detect_pkg_family)"
case "$family" in
    debian) ;;
    fedora)
        echo "Adding the Ookla rpm repository (packagecloud) and installing speedtest..."
        if curl -fsSL https://packagecloud.io/install/repositories/ookla/speedtest-cli/script.rpm.sh | bash \
                && dnf install -y -q speedtest \
                && speedtest --version >/dev/null 2>&1; then
            echo "speedtest CLI installed: $(speedtest --version | head -1)"
            exit 0
        fi
        echo "error: rpm install failed — install manually: https://www.speedtest.net/apps/cli" >&2
        exit 1 ;;
    *)
        echo "Programmatic speedtest install covers the Debian and RPM families." >&2
        echo "Install the Ookla speedtest CLI manually for your distro" >&2
        echo "(Arch: AUR; others: tarball): https://www.speedtest.net/apps/cli" >&2
        echo "hoptrail detects it automatically within a minute of installation." >&2
        exit 3 ;;
esac

echo "Adding the Ookla apt repository (packagecloud) and installing speedtest..."
if ! curl -fsSL https://packagecloud.io/install/repositories/ookla/speedtest-cli/script.deb.sh | bash; then
    echo "error: repository setup failed; see output above." >&2
    exit 1
fi
if ! apt-get install -y -qq speedtest; then
    # Ookla's repo lags new Ubuntu codenames (noble/24.04 at the time
    # of writing): the setup script writes a sources entry for the
    # detected codename even when that channel is empty, and the
    # install fails with "Unable to locate package". Fall back to the
    # newest LTS channel they DO publish (jammy) — the package itself
    # runs fine on newer releases.
    _ookla_list="/etc/apt/sources.list.d/ookla_speedtest-cli.list"
    _codename=""
    if [ -r /etc/os-release ]; then
        eval "$(. /etc/os-release; printf '_codename=%q\n' "${VERSION_CODENAME:-}")"
    fi
    if [ -n "$_codename" ] && [ "$_codename" != "jammy" ] && [ -f "$_ookla_list" ]; then
        echo "No speedtest package for '${_codename}' — retrying via the jammy (22.04) channel..."
        sed -i "s/ ${_codename} / jammy /g" "$_ookla_list"
        apt-get update -qq -o Dir::Etc::sourcelist="$_ookla_list" -o Dir::Etc::sourceparts="-" -o APT::Get::List-Cleanup="0"
        if ! apt-get install -y -qq speedtest; then
            echo "error: apt-get install speedtest failed even via jammy." >&2
            exit 1
        fi
    else
        echo "error: apt-get install speedtest failed." >&2
        exit 1
    fi
fi

# Smoke-test: confirm the binary runs. License/GDPR acceptance happens
# at first measurement — the daemon passes --accept-license
# --accept-gdpr.
if ! speedtest --version >/dev/null 2>&1; then
    echo "error: speedtest installed but won't run; try 'speedtest --version' by hand." >&2
    exit 1
fi
echo "speedtest CLI installed: $(speedtest --version | head -1)"
exit 0
