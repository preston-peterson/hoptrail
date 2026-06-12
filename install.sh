#!/bin/bash
# =============================================================================
# Hoptrail — Install Script
# =============================================================================
#
# Usage:
#   1. Build the binary (this dir): make build
#   2. Run:                          ./install.sh
#
# This script will:
#   - Install libcap-utils (for setcap) if missing
#   - Copy ./hoptrail to /opt/hoptrail/bin/hoptrail
#   - Apply cap_net_raw+ep so the daemon can open raw ICMP sockets
#     without running as root
#   - Create /var/lib/hoptrail/ (the SQLite data directory) owned by
#     the invoking user
#   - Create /opt/hoptrail/config.yaml from config.yaml.example if it
#     doesn't already exist (preserves operator edits on re-install)
#   - Install and enable the systemd service running as the invoking user
#   - Write /etc/sudoers.d/hoptrail so the web UI can drive the few
#     root-requiring actions (service restart, setcap after a self-update)
#     without the operator touching a terminal
#   - Start the service and print the listen URL
#
# Idempotent — re-running refreshes everything without disturbing data
# or operator config.
# =============================================================================

set -e

GREEN='\033[32m\033[1m'
RED='\033[31m\033[1m'
CYAN='\033[36m\033[1m'
YELLOW='\033[33m\033[1m'
RESET='\033[0m'

SOURCE_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALL_DIR="/opt/hoptrail"
BIN_PATH="${INSTALL_DIR}/bin/hoptrail"
DATA_DIR="/var/lib/hoptrail"

# Role selection (v0.3, step-95). The central daemon and the remote
# probe share the binary, install dir, data dir, and operator scripts;
# what differs is the systemd unit, the config file, and the
# subcommand. A box can host both roles (central + a probe reporting
# to a DIFFERENT central) — the unit names don't collide.
ROLE="central"
ADD_BANDWIDTH=false

# Probe identity, suppliable as flags (step-120) so the one-liner the
# central's UI generates installs a probe with zero prompts. Any value
# left empty falls back to the interactive prompt (or hand-editing).
ARG_PROBE_ID=""
ARG_CENTRAL=""
ARG_TOKEN=""

# --- Argument parsing (before root check so --help works as any user) ---
# while/shift rather than for-in: --id/--central/--token take values.
# Both `--flag value` and `--flag=value` spellings are accepted.
while [ $# -gt 0 ]; do
    arg="$1"
    case "$arg" in
        --probe) ROLE="probe" ;;
        --add-bandwidth) ADD_BANDWIDTH=true ;;
        --id=*)      ARG_PROBE_ID="${arg#*=}" ;;
        --central=*) ARG_CENTRAL="${arg#*=}" ;;
        --token=*)   ARG_TOKEN="${arg#*=}" ;;
        --id)      ARG_PROBE_ID="${2:-}"; shift ;;
        --central) ARG_CENTRAL="${2:-}";  shift ;;
        --token)   ARG_TOKEN="${2:-}";    shift ;;
        --help|-h)
            cat <<EOF
Hoptrail install script.

Usage:
  ./install.sh                  Install the central daemon (probe engine + web UI)
  ./install.sh --probe          Install a remote probe instead (reports to a central)
  ./install.sh --probe --id <probe-id> --central <url> --token <token>
                                Non-interactive probe install. This is the
                                command the central's web UI generates for you
                                (Settings → Probes → Add probe) — paste and run.
  ./install.sh --add-bandwidth  Install ONLY the speedtest CLI (bandwidth
                                monitoring capability) onto an existing install,
                                then exit. Ubuntu/Debian via the Ookla apt repo;
                                other distros get manual instructions.
  ./install.sh --help           This message

Prerequisites:
  - A built hoptrail binary at ${SOURCE_DIR}/hoptrail (run 'make build' first)
  - sudo access (for systemd, /opt/hoptrail, setcap)

Both roles install the binary to ${BIN_PATH} with cap_net_raw+ep and
write a systemd unit running as the invoking user:

  central:  hoptrail.service        serve --config ${INSTALL_DIR}/config.yaml
  probe:    hoptrail-probe.service  probe --config ${INSTALL_DIR}/probe.yaml

The probe role needs three deployment-specific values: its identity
(probe_id), the central's URL, and a bearer token. The easy path is
the central's web UI (Settings → Probes → Add probe), which mints the
token and prints this script's full command line. Values not given as
flags are prompted for; leave any prompt blank to fill the config in
by hand before starting the service. The probe registers itself in
the central's UI under its probe_id within one heartbeat of starting.

Re-running the script is safe — it refreshes the binary, preserves
configs and the database, and restarts the service. Installing one
role does not disturb the other's unit or config.
EOF
            exit 0 ;;
        *)
            echo "Unknown argument: $arg" >&2
            echo "Run ./install.sh --help for usage." >&2
            exit 2 ;;
    esac
    shift
done

if [ "$ROLE" = "probe" ]; then
    SERVICE_NAME="hoptrail-probe"
    CONFIG_BASENAME="probe.yaml"
    EXAMPLE_BASENAME="probe.yaml.example"
    EXEC_SUBCOMMAND="probe"
    UNIT_DESCRIPTION="Hoptrail probe — remote measurement point reporting to a central"
else
    SERVICE_NAME="hoptrail"
    CONFIG_BASENAME="config.yaml"
    EXAMPLE_BASENAME="config.yaml.example"
    EXEC_SUBCOMMAND="serve"
    UNIT_DESCRIPTION="Hoptrail — continuous traceroute and per-hop latency tracker"
fi
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

# --- Distro family detection (step-113) ---
#
# Exact ID match first, then ID_LIKE so derivatives (e.g. a niche
# Ubuntu respin reporting only ID_LIKE="ubuntu debian") still land in
# the right family. PKG_FAMILY ∈ debian|fedora|arch|opensuse|"".
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

# --- Bandwidth capability: the Ookla speedtest CLI (v0.4) ---
#
# hoptrail itself stays a single static binary; bandwidth monitoring
# shells out to the operator-installed `speedtest` CLI. The install
# logic lives in scripts/install-speedtest.sh — a root-run helper this
# script copies to root-owned /usr/local/lib/hoptrail/ and whitelists
# in the sudoers rule, so the web UI's "install the speedtest CLI"
# button (step-123) runs the exact same code as --add-bandwidth here.
# The daemon re-detects capability every 60s, so this can run any time
# — no hoptrail restart needed.
SPEEDTEST_HELPER="/usr/local/lib/hoptrail/install-speedtest.sh"

# Installs the root-owned helper scripts (speedtest CLI installer,
# local-ntfy installer) that the sudoers rule whitelists and the web
# UI's install buttons execute.
install_speedtest_helper() {
    local installed=1
    sudo install -d -m 0755 -o root -g root /usr/local/lib/hoptrail
    for _helper in install-speedtest.sh install-ntfy.sh; do
        if [ -f "${SOURCE_DIR}/scripts/${_helper}" ]; then
            sudo install -m 0755 -o root -g root \
                "${SOURCE_DIR}/scripts/${_helper}" "/usr/local/lib/hoptrail/${_helper}"
            installed=0
        fi
    done
    return $installed
}

install_speedtest() {
    if command -v speedtest >/dev/null 2>&1; then
        echo -e "  ${GREEN}✓${RESET} speedtest CLI already installed: $(speedtest --version 2>/dev/null | head -1)"
        return 0
    fi
    install_speedtest_helper || true
    if [ ! -x "$SPEEDTEST_HELPER" ]; then
        echo -e "  ${RED}✗${RESET} Helper not found at ${SPEEDTEST_HELPER} and no scripts/install-speedtest.sh in ${SOURCE_DIR}" >&2
        return 1
    fi
    if sudo "$SPEEDTEST_HELPER"; then
        echo "    Enable bandwidth monitoring from the gear icon in the web UI."
        return 0
    fi
    return 1
}

# Standalone mode: install the capability and exit. Works against an
# existing hoptrail install of either role (or even before one —
# capability detection just sits ready).
if [ "$ADD_BANDWIDTH" = true ]; then
    echo ""
    echo -e "${CYAN}Hoptrail — add bandwidth-monitoring capability${RESET}"
    echo ""
    install_speedtest
    exit $?
fi

# --- Refuse to run from inside an existing install's update/ staging dir ---
#
# Staging-dir trap: if the operator extracts a new release inside
# /opt/hoptrail/update/ and then runs ./install.sh from there, SOURCE_DIR
# resolves to the staging dir and the systemd unit would point at the wrong
# place. Refuse with a clear pointer at the real install root.
SOURCE_PARENT="$(dirname "$SOURCE_DIR")"
SOURCE_BASENAME="$(basename "$SOURCE_DIR")"
if [ "$SOURCE_BASENAME" = "update" ] \
        && [ -f "$SOURCE_PARENT/VERSION" ] \
        && [ -f "$SOURCE_PARENT/install.sh" ]; then
    echo -e "${RED}Error:${RESET} install.sh is sitting inside an existing install's update/ staging directory:" >&2
    echo "  Current location: $SOURCE_DIR" >&2
    echo "  Real install:     $SOURCE_PARENT" >&2
    echo "" >&2
    echo "To apply a staged release, run ./update.sh from the install root:" >&2
    echo "" >&2
    echo "  cd $SOURCE_PARENT && ./update.sh" >&2
    echo "" >&2
    exit 2
fi

# --- User detection (handles both ./install.sh and sudo ./install.sh) ---
USER="${SUDO_USER:-$(whoami)}"
if [ "$USER" = "root" ]; then
    echo -e "${RED}Please run as a regular user (not root).${RESET}"
    echo "The script uses sudo only where needed (binary placement, systemd, setcap)."
    exit 1
fi
if [ -n "${SUDO_USER:-}" ]; then
    echo -e "${CYAN}Note:${RESET} running via sudo. Service will be configured to run as ${USER}."
    echo "  Running as './install.sh' (no sudo) also works and is slightly cleaner —"
    echo "  the script uses sudo where needed."
    echo ""
fi

# --- Binary check ---
BIN_SOURCE="${SOURCE_DIR}/hoptrail"
if [ ! -x "$BIN_SOURCE" ]; then
    # No prebuilt binary in the source dir. Try to build it ourselves —
    # the install pattern is "operator runs one script and the install
    # bootstraps everything it needs." If the build host is missing
    # required tools, we emit a precise message about which tool is
    # missing rather than a generic "build failed."
    echo -e "${CYAN}No prebuilt binary at ${BIN_SOURCE}.${RESET}"
    echo "  Will build from source (requires make, go, npm, node)."

    MISSING=()
    for tool in make go npm node; do
        if ! command -v "$tool" >/dev/null 2>&1; then
            MISSING+=("$tool")
        fi
    done
    if [ "${#MISSING[@]}" -gt 0 ]; then
        echo -e "${RED}Error:${RESET} missing build tools: ${MISSING[*]}" >&2
        echo "" >&2
        echo "Install them before re-running:" >&2
        echo "  Debian/Ubuntu:  sudo apt-get install build-essential golang-go nodejs npm" >&2
        echo "  Fedora/RHEL:    sudo dnf install make gcc golang nodejs npm" >&2
        echo "  Arch:           sudo pacman -S --needed base-devel go nodejs npm" >&2
        echo "  openSUSE:       sudo zypper install -t pattern devel_basis && sudo zypper install go nodejs npm" >&2
        echo "" >&2
        exit 1
    fi

    echo -e "  Running ${CYAN}make build${RESET}..."
    if ! make -C "$SOURCE_DIR" build; then
        echo -e "${RED}Error:${RESET} build failed; see output above." >&2
        echo "Re-run install.sh after fixing the build errors." >&2
        exit 1
    fi

    if [ ! -x "$BIN_SOURCE" ]; then
        echo -e "${RED}Error:${RESET} build appeared to succeed but ${BIN_SOURCE} is still missing." >&2
        exit 1
    fi
    echo -e "  ${GREEN}✓${RESET} Built ${BIN_SOURCE}"
    echo ""
fi

echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════╗${RESET}"
echo -e "${CYAN}║         Hoptrail — Install                   ║${RESET}"
echo -e "${CYAN}╚══════════════════════════════════════════════╝${RESET}"
echo ""
echo "  Role:         ${ROLE}"
echo "  Source dir:   ${SOURCE_DIR}"
echo "  Install dir:  ${INSTALL_DIR}"
echo "  Data dir:     ${DATA_DIR}"
echo "  Service:      ${SERVICE_NAME}.service"
echo "  Run as user:  ${USER}"
echo ""

# --- [1/6] System dependencies ---
#
# Hoptrail's only system dependency at install time is `setcap` from
# libcap-utils. Every modern Linux ships it under one of three package
# names depending on the distro family. The check-then-install pattern
# keeps re-runs quiet when it's already there.
echo -e "${CYAN}[1/6]${RESET} Checking system dependencies..."
if command -v setcap >/dev/null 2>&1; then
    echo -e "  ${GREEN}✓${RESET} setcap already present"
else
    echo -e "  ${YELLOW}·${RESET} setcap missing — installing libcap-utils"
    case "$(detect_pkg_family)" in
        debian)
            sudo apt-get update -qq && sudo apt-get install -y -qq libcap2-bin ;;
        fedora)
            sudo dnf install -y -q libcap ;;
        arch)
            sudo pacman -S --needed --noconfirm libcap ;;
        opensuse)
            sudo zypper --non-interactive install libcap-progs ;;
        *)
            echo -e "  ${RED}✗${RESET} Unknown distro — install setcap manually (libcap2-bin, libcap, or libcap-progs)" >&2
            exit 1 ;;
    esac
    if ! command -v setcap >/dev/null 2>&1; then
        echo -e "  ${RED}✗${RESET} setcap still not on PATH after install" >&2
        exit 1
    fi
    echo -e "  ${GREEN}✓${RESET} setcap installed"
fi

# --- [2/6] Install directory + binary ---
echo -e "${CYAN}[2/6]${RESET} Installing binary..."
sudo install -d -m 0755 -o "$USER" -g "$USER" "${INSTALL_DIR}"
sudo install -d -m 0755 -o "$USER" -g "$USER" "${INSTALL_DIR}/bin"
sudo install -d -m 0755 -o "$USER" -g "$USER" "${INSTALL_DIR}/.backups"
sudo install -d -m 0755 -o "$USER" -g "$USER" "${INSTALL_DIR}/update"

# Copy the binary into place. `install -m 0755` is preferable to `cp`
# here because it does an atomic rename — if the daemon is currently
# running, the running process keeps the old inode and continues
# uninterrupted; only the next start picks up the new binary. (The
# systemctl restart below is what actually swaps the running version.)
sudo install -m 0755 -o "$USER" -g "$USER" "$BIN_SOURCE" "$BIN_PATH"

# Apply cap_net_raw+ep. This MUST come after every binary replacement —
# capability bits live on the inode, and `install` produces a new inode.
# (Lesson #7 from the project handoff.)
sudo setcap cap_net_raw+ep "$BIN_PATH"

# Sanity-check the cap actually stuck. nosuid mounts silently drop
# setcap; /opt isn't typically nosuid but the check costs nothing.
if ! sudo getcap "$BIN_PATH" | grep -q "cap_net_raw"; then
    echo -e "  ${RED}✗${RESET} setcap appeared to succeed but cap_net_raw is not set on ${BIN_PATH}" >&2
    echo "    Filesystem may be mounted nosuid. Try a different install location." >&2
    exit 1
fi
echo -e "  ${GREEN}✓${RESET} Binary installed at ${BIN_PATH} with cap_net_raw+ep"

# Operator scripts live alongside the install so the documented
# `/opt/hoptrail/update.sh --staged` flow works on every fresh install
# without an operator needing to keep the source tree around.
for _script in update.sh uninstall.sh; do
    if [ -f "${SOURCE_DIR}/${_script}" ]; then
        sudo install -m 0755 -o "$USER" -g "$USER" \
            "${SOURCE_DIR}/${_script}" "${INSTALL_DIR}/${_script}"
        echo -e "  ${GREEN}✓${RESET} Installed ${INSTALL_DIR}/${_script}"
    fi
done

# The speedtest install helper goes to root-owned /usr/local/lib/ on
# every install (step-123) — it's what the sudoers rule whitelists and
# what the web UI's "install the speedtest CLI" button executes. Tiny,
# inert until invoked; installing it unconditionally means the button
# works even when the operator skipped the bandwidth prompt below.
if install_speedtest_helper; then
    echo -e "  ${GREEN}✓${RESET} Installed ${SPEEDTEST_HELPER}"
fi

# --- [3/6] Config + data directory ---
echo -e "${CYAN}[3/6]${RESET} Setting up config and data directories..."

# Config: copy from example only if not present (preserves operator edits).
# START_SERVICE gates [5/5]: a probe whose identity/central/token were
# never filled in must NOT be started — it would either crash-loop
# (unresolvable placeholder central) or, worse, sit "active" while
# never registering. Lesson #9: don't run a service that can't do its
# job.
CONFIG_PATH="${INSTALL_DIR}/${CONFIG_BASENAME}"
START_SERVICE=true
if [ ! -f "$CONFIG_PATH" ]; then
    if [ -f "${SOURCE_DIR}/${EXAMPLE_BASENAME}" ]; then
        sudo install -m 0644 -o "$USER" -g "$USER" \
            "${SOURCE_DIR}/${EXAMPLE_BASENAME}" "$CONFIG_PATH"
        echo -e "  ${GREEN}✓${RESET} Created ${CONFIG_PATH} from example"
    else
        echo -e "  ${YELLOW}⚠${RESET}  No ${EXAMPLE_BASENAME} in source dir — service may not start"
    fi

    # Probe role, fresh config: the example ships with placeholders
    # that can never work (probe_id, central URL, token are
    # deployment-specific). Prompt for them when interactive; sed the
    # answers in; validate with the binary's own checker. Any blank
    # answer = operator will edit by hand → install everything but
    # leave the service stopped with clear next steps.
    if [ "$ROLE" = "probe" ]; then
        # Flag-supplied values (the UI-generated one-liner) win; any
        # still-empty value is prompted for when interactive.
        PROBE_ID="$ARG_PROBE_ID" CENTRAL_URL="$ARG_CENTRAL" PROBE_TOKEN="$ARG_TOKEN"
        if [ -t 0 ] && { [ -z "$PROBE_ID" ] || [ -z "$CENTRAL_URL" ] || [ -z "$PROBE_TOKEN" ]; }; then
            echo ""
            echo "  A probe needs three deployment-specific values. The central's"
            echo "  web UI generates this script's full command (Settings → Probes"
            echo "  → Add probe). Leave any prompt blank to skip and edit"
            echo "  ${CONFIG_PATH} by hand instead."
            echo ""
            [ -z "$PROBE_ID" ]    && read -r -p "  probe_id (kebab-case, e.g. site-east-pi): " PROBE_ID
            [ -z "$CENTRAL_URL" ] && read -r -p "  central URL (e.g. http://192.0.2.10:8080): " CENTRAL_URL
            [ -z "$PROBE_TOKEN" ] && read -r -p "  token (from Settings → Probes on the central): " PROBE_TOKEN
            echo ""
        fi
        if [ -n "$PROBE_ID" ] && [ -n "$CENTRAL_URL" ] && [ -n "$PROBE_TOKEN" ]; then
            # `|` as the sed delimiter: URLs contain `/`, tokens are
            # base64url (A-Za-z0-9_-) — neither can contain `|`.
            sudo sed -i \
                -e "s|^probe_id:.*|probe_id: \"${PROBE_ID}\"|" \
                -e "s|^  url:.*|  url: \"${CENTRAL_URL}\"|" \
                -e "s|^  token:.*|  token: \"${PROBE_TOKEN}\"|" \
                "$CONFIG_PATH"
            if "$BIN_PATH" check-config --probe --config "$CONFIG_PATH" >/dev/null 2>&1; then
                echo -e "  ${GREEN}✓${RESET} Probe config written and validated"
            else
                echo -e "  ${RED}✗${RESET} Config did not validate:"
                "$BIN_PATH" check-config --probe --config "$CONFIG_PATH" || true
                echo "    Fix ${CONFIG_PATH}, then: sudo systemctl start ${SERVICE_NAME}"
                START_SERVICE=false
            fi
        else
            echo -e "  ${YELLOW}·${RESET} Probe identity not provided — service installed but NOT started."
            echo "    Edit ${CONFIG_PATH} (probe_id, central.url, central.token), then:"
            echo "      sudo systemctl enable --now ${SERVICE_NAME}"
            START_SERVICE=false
        fi
    fi
else
    echo -e "  ${GREEN}✓${RESET} Existing ${CONFIG_PATH} preserved"
fi

# Data dir: hoptrail's storage layer auto-mkdir's this on first run (step-8),
# but creating it upfront with the right ownership avoids the daemon ever
# running as a user that lacks write access to /var/lib/.
sudo install -d -m 0755 -o "$USER" -g "$USER" "$DATA_DIR"
echo -e "  ${GREEN}✓${RESET} Data directory ${DATA_DIR} (owned by ${USER})"

# --- Bandwidth prompt (central role, interactive, CLI not present) ---
#
# The v0.4 opt-in disclosure (design §4.1): offered, never assumed.
# Tests are OFF by default even with the CLI installed — enabling is
# a separate, explicit step in the web UI's settings panel.
if [ "$ROLE" = "central" ] && [ -t 0 ] && ! command -v speedtest >/dev/null 2>&1; then
    echo -e "${CYAN}[opt]${RESET} Bandwidth monitoring (optional)..."
    echo "  hoptrail can run scheduled speed tests and alert when your ISP"
    echo "  derates your link. Each test transfers ~250 MB on gigabit; tests"
    echo "  are OFF by default and scheduled from the web UI when you enable"
    echo "  them. This step only installs the Ookla speedtest CLI."
    read -r -p "  Install the speedtest CLI now? [y/N] " _bw
    if [[ "$_bw" =~ ^[Yy]$ ]]; then
        install_speedtest || true
    else
        echo -e "  ${YELLOW}·${RESET} Skipped. Add it later with: ./install.sh --add-bandwidth"
    fi
fi

# --- [4/6] Systemd unit ---
echo -e "${CYAN}[4/6]${RESET} Installing systemd service..."
sudo tee "$SERVICE_FILE" > /dev/null << SERVICEEOF
[Unit]
Description=${UNIT_DESCRIPTION}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${USER}
Group=${USER}
WorkingDirectory=${INSTALL_DIR}
ExecStart=${BIN_PATH} ${EXEC_SUBCOMMAND} --config ${CONFIG_PATH}
Restart=on-failure
RestartSec=15
StandardOutput=journal
StandardError=journal
TimeoutStopSec=10

[Install]
WantedBy=multi-user.target
SERVICEEOF

sudo systemctl daemon-reload
if [ "$START_SERVICE" = true ]; then
    sudo systemctl enable "$SERVICE_NAME" >/dev/null 2>&1
    echo -e "  ${GREEN}✓${RESET} Service installed and enabled"
else
    echo -e "  ${GREEN}✓${RESET} Service installed (not enabled — config needs values first)"
fi

# --- [5/6] Sudoers rule for UI-driven privileged actions (step-119) ---
#
# Operational simplicity: the web UI must be able to perform the few
# root-requiring actions (restart the service after a
# settings change or self-update; re-apply cap_net_raw after the binary
# is replaced — capability bits die with the inode, lesson #7) without
# the operator running one-off CLI commands.
#
# The whitelist is exact commands with exact arguments — no wildcards.
# Both /bin and /usr/bin (/sbin and /usr/sbin) spellings are listed
# because usrmerge differs across distros. The SUDOERS_VERSION marker
# is machine-readable: a future in-UI updater compares the live file's
# version against the release's expectation and blocks the update with
# a "re-run install.sh" message when this file needs refreshing.
#
# The speedtest helper path points into root-owned /usr/local/lib/ —
# deliberately NOT user-owned /opt/hoptrail/ — so the service user
# cannot swap the file behind the sudo rule. (The helper itself ships
# with the bandwidth-install-from-UI feature; until then the rule is
# inert.)
echo -e "${CYAN}[5/6]${RESET} Installing sudoers rule..."
SUDOERS_FILE="/etc/sudoers.d/hoptrail"
SUDOERS_TMP="$(mktemp)"
cat > "$SUDOERS_TMP" << SUDOERSEOF
# SUDOERS_VERSION: 2
# Hoptrail sudoers rule — machine-readable. Written by install.sh; the
# version marker is compared by the in-UI updater to detect when a new
# release needs this file refreshed (re-run install.sh to refresh).
# Never edit this file by hand.

# Restart the services: used by the UI's restart button and automatic
# post-update restarts. Both roles listed unconditionally — the rule
# for a unit that isn't installed is harmless, and keeping the file
# identical across roles means SUDOERS_VERSION needs no role branches.
${USER} ALL=(ALL) NOPASSWD: /bin/systemctl restart hoptrail, /usr/bin/systemctl restart hoptrail, /bin/systemctl restart --no-block hoptrail, /usr/bin/systemctl restart --no-block hoptrail
${USER} ALL=(ALL) NOPASSWD: /bin/systemctl restart hoptrail-probe, /usr/bin/systemctl restart hoptrail-probe, /bin/systemctl restart --no-block hoptrail-probe, /usr/bin/systemctl restart --no-block hoptrail-probe

# Re-apply the raw-ICMP capability after a self-update replaces the
# binary (capability bits are an inode property and do not survive the
# replacement). Exact path, exact capability string.
${USER} ALL=(ALL) NOPASSWD: /sbin/setcap cap_net_raw+ep ${BIN_PATH}, /usr/sbin/setcap cap_net_raw+ep ${BIN_PATH}

# Install the Ookla speedtest CLI when the operator clicks the install
# button in the bandwidth settings (root-owned helper script).
${USER} ALL=(ALL) NOPASSWD: /usr/local/lib/hoptrail/install-speedtest.sh

# Install a local ntfy server when the operator clicks the install
# button in the alerts settings (v0.6; root-owned helper script).
${USER} ALL=(ALL) NOPASSWD: /usr/local/lib/hoptrail/install-ntfy.sh

# Read this file back so the updater can verify the live
# SUDOERS_VERSION against a staged release's expectation. Read-only.
${USER} ALL=(ALL) NOPASSWD: /bin/cat /etc/sudoers.d/hoptrail, /usr/bin/cat /etc/sudoers.d/hoptrail
SUDOERSEOF

# Validate before installing — a malformed file in /etc/sudoers.d can
# break sudo for the whole box. visudo -cf parses without installing.
if command -v visudo >/dev/null 2>&1; then
    if ! visudo -cf "$SUDOERS_TMP" >/dev/null 2>&1; then
        echo -e "  ${RED}✗${RESET} Generated sudoers rule failed visudo validation — not installed." >&2
        echo "    This is a bug; please report it. UI-driven restart/update will prompt for sudo instead." >&2
        rm -f "$SUDOERS_TMP"
        exit 1
    fi
else
    echo -e "  ${YELLOW}·${RESET} visudo not found — installing sudoers rule without validation"
fi
sudo install -m 0440 -o root -g root "$SUDOERS_TMP" "$SUDOERS_FILE"
rm -f "$SUDOERS_TMP"
echo -e "  ${GREEN}✓${RESET} ${SUDOERS_FILE} installed (version 2 — UI restart/update/installers enabled for ${USER})"

# --- [6/6] Start the service + verify ---
echo -e "${CYAN}[6/6]${RESET} Starting the service..."
if [ "$START_SERVICE" = true ]; then
    # `restart` rather than `start` handles the re-install case where the
    # service is already running with the old binary. systemd will stop
    # the old daemon, then start the new one.
    sudo systemctl restart "$SERVICE_NAME"
    sleep 2

    if sudo systemctl is-active --quiet "$SERVICE_NAME"; then
        echo -e "  ${GREEN}✓${RESET} Service is running"
    else
        echo -e "  ${RED}✗${RESET} Service failed to start. Check:"
        echo "      sudo journalctl -u ${SERVICE_NAME} -n 50"
        exit 1
    fi
else
    echo -e "  ${YELLOW}·${RESET} Skipped (config incomplete — see step 3/5)"
fi

echo ""
echo -e "${GREEN}══════════════════════════════════════════════${RESET}"
echo -e "${GREEN}  Install complete${RESET}"
echo -e "${GREEN}══════════════════════════════════════════════${RESET}"
echo ""
if [ "$ROLE" = "probe" ]; then
    if [ "$START_SERVICE" = true ]; then
        echo "  This probe registers itself in the central's UI (the probe"
        echo "  picker in the top bar) within one heartbeat — no central-side"
        echo "  setup beyond its token being in probes.tokens."
    else
        echo "  Finish the config, then start the probe:"
        echo "    \$EDITOR ${CONFIG_PATH}"
        echo "    sudo systemctl enable --now ${SERVICE_NAME}"
    fi
else
    # --- Self-IP for the welcome banner ---
    #
    # `ip route get` is the cross-distro way to learn which local IP
    # would route to the internet. `hostname -I` is a Debian extension;
    # on Arch it silently returns nothing. Fallback chain handles
    # minimal containers.
    SERVER_IP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '/src/{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')
    [ -z "$SERVER_IP" ] && SERVER_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
    [ -z "$SERVER_IP" ] && SERVER_IP="localhost"

    # Pull the listen address from the config so the banner shows the
    # actual port. Falls back to :8080 if parsing fails.
    LISTEN=$(grep -E '^\s*listen:' "$CONFIG_PATH" 2>/dev/null | head -1 | sed -E 's/.*listen:\s*"?([^"]+)"?.*/\1/')
    [ -z "$LISTEN" ] && LISTEN=":8080"
    PORT="${LISTEN##*:}"
    echo "  Web UI:  http://${SERVER_IP}:${PORT}"
fi
echo ""
echo "  Commands:"
echo "    sudo systemctl status ${SERVICE_NAME}"
echo "    sudo systemctl restart ${SERVICE_NAME}"
echo "    sudo systemctl stop ${SERVICE_NAME}"
echo "    sudo journalctl -u ${SERVICE_NAME} -f"
echo ""
echo "  Config:  ${CONFIG_PATH}"
if [ "$ROLE" = "probe" ]; then
    echo "  Buffer:  ${DATA_DIR}/probe-buffer.db (partition spill)"
else
    echo "  Data:    ${DATA_DIR}/hoptrail.db"
fi
echo ""
