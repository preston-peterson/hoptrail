#!/bin/bash
# =============================================================================
# Hoptrail — Uninstall Script
# =============================================================================
#
# Removes Hoptrail from the system, reversing what install.sh set up.
#
# Usage:
#   ./uninstall.sh           # Interactive — prompts before deleting data
#   ./uninstall.sh --purge   # Remove everything (data included) without prompts
#   ./uninstall.sh --keep    # Keep all data files, no prompts (service only)
#
# What gets removed (always):
#   - systemd service (stopped, disabled, unit file deleted)
#   - The sudoers rule (/etc/sudoers.d/hoptrail)
#   - The installed binary (/opt/hoptrail/bin/hoptrail)
#
# What you're prompted about:
#   - Config file (/opt/hoptrail/config.yaml)
#   - Database (/var/lib/hoptrail/hoptrail.db + WAL/SHM sidecars)
#   - The data directory itself (/var/lib/hoptrail/)
#   - The install directory itself (/opt/hoptrail/)
#
# What is NEVER removed:
#   - System packages (libcap2-bin) — might be used by other apps
#   - Anything outside /opt/hoptrail/ and /var/lib/hoptrail/
# =============================================================================

set -e

GREEN='\033[32m\033[1m'
RED='\033[31m\033[1m'
CYAN='\033[36m\033[1m'
YELLOW='\033[33m\033[1m'
RESET='\033[0m'

INSTALL_DIR="/opt/hoptrail"
BIN_PATH="${INSTALL_DIR}/bin/hoptrail"
DATA_DIR="/var/lib/hoptrail"
# Both roles' units are removed if present (step-95: a box can host
# the central, a remote probe, or both — uninstall takes down whatever
# is here).
SERVICE_NAMES=("hoptrail" "hoptrail-probe")

PURGE=false
KEEP=false
for arg in "$@"; do
    case "$arg" in
        --purge) PURGE=true ;;
        --keep)  KEEP=true ;;
        --help|-h)
            sed -n '3,25p' "$0" | sed -e '/^# =\+$/d' -e 's/^# \?//'
            exit 0 ;;
        *)
            echo -e "${RED}Unknown option: $arg${RESET}" >&2
            echo "Run with --help for usage" >&2
            exit 1 ;;
    esac
done

if [ "$PURGE" = true ] && [ "$KEEP" = true ]; then
    echo -e "${RED}Cannot use --purge and --keep together${RESET}" >&2
    exit 1
fi

echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════╗${RESET}"
echo -e "${CYAN}║         Hoptrail — Uninstall                 ║${RESET}"
echo -e "${CYAN}╚══════════════════════════════════════════════╝${RESET}"
echo ""
echo "  Install dir:  ${INSTALL_DIR}"
echo "  Data dir:     ${DATA_DIR}"
echo "  Services:     ${SERVICE_NAMES[*]} (whichever are installed)"
echo ""

# --- Confirm in interactive mode ---
if [ "$PURGE" = false ] && [ "$KEEP" = false ]; then
    read -p "Are you sure you want to uninstall Hoptrail? [y/N] " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "Cancelled."
        exit 0
    fi
fi

# --- [1/4] Stop and remove systemd services ---
echo -e "${CYAN}[1/4]${RESET} Removing systemd services..."
REMOVED_ANY=false
for svc in "${SERVICE_NAMES[@]}"; do
    if sudo systemctl list-unit-files 2>/dev/null | grep -q "^${svc}.service"; then
        sudo systemctl stop "$svc" 2>/dev/null || true
        sudo systemctl disable "$svc" 2>/dev/null || true
        sudo rm -f "/etc/systemd/system/${svc}.service"
        REMOVED_ANY=true
        echo -e "  ${GREEN}✓${RESET} ${svc} stopped, disabled, and unit file removed"
    fi
done
if [ "$REMOVED_ANY" = true ]; then
    sudo systemctl daemon-reload
    sudo systemctl reset-failed 2>/dev/null || true
else
    echo -e "  ${YELLOW}·${RESET} No systemd services found (already removed or never installed)"
fi

# The sudoers rule is removed unconditionally (step-119) — it exists
# only to let the UI drive restarts/updates, which are gone with the
# services. Never prompted: leaving a stale NOPASSWD rule behind is
# strictly worse than removing it.
if [ -f /etc/sudoers.d/hoptrail ]; then
    sudo rm -f /etc/sudoers.d/hoptrail
    echo -e "  ${GREEN}✓${RESET} Sudoers rule /etc/sudoers.d/hoptrail removed"
fi

# A hoptrail-managed local ntfy server (the alerts install button
# stamps /etc/ntfy/hoptrail-installed) is removed with the rest; an
# operator's own ntfy (no stamp) is never touched.
if [ -f /etc/ntfy/hoptrail-installed ]; then
    sudo systemctl stop ntfy 2>/dev/null || true
    sudo systemctl disable ntfy 2>/dev/null || true
    sudo rm -f /usr/local/bin/ntfy /etc/ntfy/server.yml /etc/systemd/system/ntfy.service /etc/ntfy/hoptrail-installed
    sudo rmdir --ignore-fail-on-non-empty /etc/ntfy 2>/dev/null || true
    sudo systemctl daemon-reload
    echo -e "  ${GREEN}✓${RESET} hoptrail-managed ntfy server removed"
fi

# The root-owned speedtest install helper (step-123) goes with the
# sudoers rule that whitelists it. The speedtest CLI itself is a
# system package and is intentionally left installed (listed in the
# NOT-removed hints below).
if [ -d /usr/local/lib/hoptrail ]; then
    sudo rm -f /usr/local/lib/hoptrail/install-speedtest.sh /usr/local/lib/hoptrail/install-ntfy.sh
    sudo rmdir --ignore-fail-on-non-empty /usr/local/lib/hoptrail
    echo -e "  ${GREEN}✓${RESET} Speedtest install helper removed"
fi

# --- [2/4] Remove the binary ---
echo -e "${CYAN}[2/4]${RESET} Removing binary..."
if [ -f "$BIN_PATH" ]; then
    sudo rm -f "$BIN_PATH"
    echo -e "  ${GREEN}✓${RESET} ${BIN_PATH} removed"
    # Try to remove the now-empty bin/ dir — best-effort, won't fail if
    # something else is in it.
    sudo rmdir "${INSTALL_DIR}/bin" 2>/dev/null || true
else
    echo -e "  ${YELLOW}·${RESET} No binary at ${BIN_PATH}"
fi

# --- [3/4] Handle data files (prompt or flag-driven) ---
echo -e "${CYAN}[3/4]${RESET} Handling data and config..."

# remove_or_keep prompts (or honors --purge/--keep) for a single path.
# Shows the path's size before the prompt so the operator knows what's
# being asked about. `sudo` on the rm because /var/lib/ paths may be
# owned in ways the invoking user can't directly remove.
remove_or_keep() {
    local path="$1"
    local description="$2"

    if [ ! -e "$path" ]; then
        echo -e "  ${YELLOW}·${RESET} ${description}: not present"
        return
    fi

    if [ "$PURGE" = true ]; then
        sudo rm -rf "$path"
        echo -e "  ${GREEN}✓${RESET} ${description}: removed (--purge)"
        return
    fi

    if [ "$KEEP" = true ]; then
        echo -e "  ${YELLOW}·${RESET} ${description}: kept (--keep)"
        return
    fi

    # Interactive prompt with size display.
    local size=""
    if [ -f "$path" ]; then
        size=" ($(sudo du -h "$path" 2>/dev/null | cut -f1))"
    elif [ -d "$path" ]; then
        size=" ($(sudo du -sh "$path" 2>/dev/null | cut -f1))"
    fi
    read -p "  Delete ${description}${size}? [y/N] " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        sudo rm -rf "$path"
        echo -e "    ${GREEN}✓${RESET} Removed"
    else
        echo -e "    ${YELLOW}·${RESET} Kept"
    fi
}

remove_or_keep "${INSTALL_DIR}/config.yaml" "Config file (config.yaml)"
remove_or_keep "${INSTALL_DIR}/probe.yaml" "Probe config (probe.yaml)"
remove_or_keep "${DATA_DIR}/hoptrail.db" "Database (hoptrail.db)"
remove_or_keep "${DATA_DIR}/hoptrail.db-wal" "Database WAL file"
remove_or_keep "${DATA_DIR}/hoptrail.db-shm" "Database SHM file"
remove_or_keep "${DATA_DIR}/probe-buffer.db" "Probe spill buffer (probe-buffer.db)"

# Once the DB-and-sidecars decisions are done, the data dir itself may
# now be empty — offer to remove it. If the operator chose to keep the
# DB, the dir will be non-empty and `rm -rf` on it would delete the DB
# anyway, so skip the prompt in that case.
if [ -d "$DATA_DIR" ]; then
    if [ -z "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
        remove_or_keep "$DATA_DIR" "Data directory (now empty)"
    else
        echo -e "  ${YELLOW}·${RESET} Data directory ${DATA_DIR} not empty — kept"
    fi
fi

# --- [4/4] Offer to remove the install directory itself ---
echo -e "${CYAN}[4/4]${RESET} Install directory..."

if [ -d "$INSTALL_DIR" ]; then
    # The install dir holds: bin/ (likely gone now), .backups/, update/,
    # and possibly the config we just removed or kept. Offer to remove
    # the whole tree; the operator can also choose to keep .backups for
    # potential rollback.
    remove_or_keep "$INSTALL_DIR" "Install directory (${INSTALL_DIR})"
else
    echo -e "  ${YELLOW}·${RESET} ${INSTALL_DIR} not present"
fi

# --- Done ---
echo ""
echo -e "${GREEN}══════════════════════════════════════════════${RESET}"
echo -e "${GREEN}  Uninstall complete${RESET}"
echo -e "${GREEN}══════════════════════════════════════════════${RESET}"
echo ""
echo "  Hoptrail has been removed from this system."
echo ""

# Informational hint about what's intentionally left behind.
_os_id=""
if [ -r /etc/os-release ]; then
    eval "$(. /etc/os-release; printf '_os_id=%q\n' "${ID:-}")"
fi
case "$_os_id" in
    debian|ubuntu|raspbian|linuxmint|pop|elementary|neon|kali|parrot)
        _pkg_remove="sudo apt-get remove libcap2-bin"
        _pkg_name="libcap2-bin" ;;
    fedora|rhel|centos|rocky|almalinux|amzn|ol)
        _pkg_remove="sudo dnf remove libcap"
        _pkg_name="libcap" ;;
    arch|manjaro|endeavouros|garuda|artix|cachyos)
        _pkg_remove="sudo pacman -R libcap"
        _pkg_name="libcap" ;;
    opensuse*|sles|sled)
        _pkg_remove="sudo zypper remove libcap-progs"
        _pkg_name="libcap-progs" ;;
    *)
        _pkg_remove="(use your distro's package manager)"
        _pkg_name="libcap utilities" ;;
esac

echo "  NOT removed (intentional):"
echo "    - System packages (${_pkg_name})"
echo "      Remove manually if no other apps need setcap: ${_pkg_remove}"
echo "    - The Ookla speedtest CLI, if installed (a system package;"
echo "      remove with your package manager: package name 'speedtest')"
echo ""
echo "  To reinstall, run ./install.sh from a fresh release tarball."
echo ""
