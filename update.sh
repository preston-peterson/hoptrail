#!/bin/bash
# =============================================================================
# Hoptrail — Update Script
# =============================================================================
#
# Replaces the installed binary with a newer one, preserving config and data.
#
# Usage:
#   1. Build the new binary on this dev box: make build
#      (this produces ./hoptrail in the current dir)
#   2. Run:                                  ./update.sh
#
# OR for a staged update on the server itself:
#   1. SCP/rsync a new hoptrail binary to /opt/hoptrail/update/hoptrail
#   2. Run on the server:                    /opt/hoptrail/update.sh --staged
#
# This script will:
#   - Locate the new binary (./hoptrail or ${INSTALL_DIR}/update/hoptrail)
#   - Stop the running service
#   - Back up the current binary to ${INSTALL_DIR}/.backups/<timestamp>/
#   - Atomically replace ${BIN_PATH} with the new binary
#   - Re-apply cap_net_raw+ep (every binary swap strips capability bits)
#   - Restart the service and verify it came up
#
# What gets preserved:
#   - /opt/hoptrail/config.yaml (operator-edited)
#   - /var/lib/hoptrail/*       (database + sidecars)
#   - /opt/hoptrail/.backups/*  (prior versions for rollback)
#
# What gets replaced:
#   - /opt/hoptrail/bin/hoptrail (with a timestamped backup made first)
# =============================================================================

set -e

GREEN='\033[32m\033[1m'
RED='\033[31m\033[1m'
CYAN='\033[36m\033[1m'
YELLOW='\033[33m\033[1m'
RESET='\033[0m'

INSTALL_DIR="/opt/hoptrail"
BIN_PATH="${INSTALL_DIR}/bin/hoptrail"
BACKUPS_DIR="${INSTALL_DIR}/.backups"

# Both roles (central `hoptrail`, remote probe `hoptrail-probe`) share
# the binary at BIN_PATH. An update must bounce every installed role —
# whichever units exist on this box. (Step-95; before that only the
# central unit existed.)
SERVICES=()
[ -f /etc/systemd/system/hoptrail.service ] && SERVICES+=("hoptrail")
[ -f /etc/systemd/system/hoptrail-probe.service ] && SERVICES+=("hoptrail-probe")
# Pre-step-95 installs have the unit but this script predates the
# detection logic — fall back to the central unit.
[ "${#SERVICES[@]}" -eq 0 ] && SERVICES=("hoptrail")
STAGED_BIN="${INSTALL_DIR}/update/hoptrail"
SOURCE_DIR="$(cd "$(dirname "$0")" && pwd)"
LOCAL_BIN="${SOURCE_DIR}/hoptrail"

STAGED=false
for arg in "$@"; do
    case "$arg" in
        --staged) STAGED=true ;;
        --help|-h)
            sed -n '3,31p' "$0" | sed -e '/^# =\+$/d' -e 's/^# \?//'
            exit 0 ;;
        *)
            echo -e "${RED}Unknown option: $arg${RESET}" >&2
            echo "Run with --help for usage" >&2
            exit 1 ;;
    esac
done

# --- Locate the new binary ---
#
# Two modes: "I just built a new binary in the dev dir, push it" (default),
# or "I rsync'd a new binary to the staging dir on the server itself"
# (--staged). The split keeps both workflows honest about which file is
# being applied, rather than auto-resolving and surprising the operator.
if [ "$STAGED" = true ]; then
    NEW_BIN="$STAGED_BIN"
    if [ ! -f "$NEW_BIN" ]; then
        echo -e "${RED}Error:${RESET} no staged binary at ${NEW_BIN}" >&2
        echo "" >&2
        echo "To stage an update, copy a new hoptrail binary to ${STAGED_BIN}," >&2
        echo "then run ${INSTALL_DIR}/update.sh --staged" >&2
        echo "" >&2
        exit 1
    fi
else
    NEW_BIN="$LOCAL_BIN"
    if [ ! -x "$NEW_BIN" ]; then
        echo -e "${RED}Error:${RESET} ${NEW_BIN} not found or not executable." >&2
        echo "" >&2
        echo "Build it first:" >&2
        echo "  cd $SOURCE_DIR" >&2
        echo "  make build" >&2
        echo "" >&2
        echo "Or apply a staged binary at ${STAGED_BIN} with: ./update.sh --staged" >&2
        echo "" >&2
        exit 1
    fi
fi

# --- Verify install exists ---
if [ ! -f "$BIN_PATH" ]; then
    echo -e "${RED}Error:${RESET} no existing install at ${BIN_PATH}" >&2
    echo "Run ./install.sh first for a fresh install." >&2
    exit 1
fi

# --- User detection ---
USER="${SUDO_USER:-$(whoami)}"

# --- Version detection (for the banner — best-effort, won't fail update) ---
CURRENT_VERSION=$("$BIN_PATH" version 2>/dev/null | head -1 || echo "unknown")
NEW_VERSION=$("$NEW_BIN" version 2>/dev/null | head -1 || echo "unknown")

echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════╗${RESET}"
echo -e "${CYAN}║         Hoptrail — Update                    ║${RESET}"
echo -e "${CYAN}╚══════════════════════════════════════════════╝${RESET}"
echo ""
echo "  Source:       ${NEW_BIN}"
echo "  Target:       ${BIN_PATH}"
echo "  Current:      ${CURRENT_VERSION}"
echo "  New:          ${NEW_VERSION}"
echo "  Services:     ${SERVICES[*]}"
echo ""

# WAS_ACTIVE remembers which services were running before the swap so
# we only restart those — an intentionally-stopped unit (e.g. a probe
# awaiting config) stays stopped.
WAS_ACTIVE=()

# --- [1/4] Stop the services ---
echo -e "${CYAN}[1/4]${RESET} Stopping services..."
for svc in "${SERVICES[@]}"; do
    if sudo systemctl is-active --quiet "$svc"; then
        sudo systemctl stop "$svc"
        WAS_ACTIVE+=("$svc")
        echo -e "  ${GREEN}✓${RESET} ${svc} stopped"
    else
        echo -e "  ${YELLOW}·${RESET} ${svc} was not running"
    fi
done

# --- [2/4] Backup the current binary ---
echo -e "${CYAN}[2/4]${RESET} Backing up current binary..."
TS=$(date +%Y%m%d-%H%M%S)
BACKUP_DIR="${BACKUPS_DIR}/${TS}"
sudo install -d -m 0755 -o "$USER" -g "$USER" "$BACKUP_DIR"
sudo install -m 0755 -o "$USER" -g "$USER" "$BIN_PATH" "${BACKUP_DIR}/hoptrail"
echo -e "  ${GREEN}✓${RESET} Backed up to ${BACKUP_DIR}/hoptrail"

# Keep only the 5 most-recent backups. Backups are timestamps (sorted
# lexically = chronologically), so head/tail with -n +6 trims the
# older entries.
BACKUP_KEEP=5
OLD_BACKUPS=$(sudo ls -1 "$BACKUPS_DIR" 2>/dev/null | sort -r | tail -n +$((BACKUP_KEEP + 1)) || true)
if [ -n "$OLD_BACKUPS" ]; then
    echo "$OLD_BACKUPS" | while read -r old; do
        sudo rm -rf "${BACKUPS_DIR}/${old}"
    done
    echo -e "  ${YELLOW}·${RESET} Pruned older backups (keeping latest ${BACKUP_KEEP})"
fi

# --- [3/4] Atomically replace the binary + re-apply setcap ---
echo -e "${CYAN}[3/4]${RESET} Installing new binary..."

# `install -m` does an atomic rename on the same filesystem — much
# safer than `cp` for a binary swap. The kernel's open-file semantics
# mean a running daemon (we just stopped it, but in case Restart=on-failure
# fires) would keep the old inode anyway, but atomic is better hygiene.
sudo install -m 0755 -o "$USER" -g "$USER" "$NEW_BIN" "$BIN_PATH"

# CRITICAL: re-apply cap_net_raw+ep. The new binary is a new inode and
# inherits zero capabilities. Without this step the next service start
# would fail to open the raw ICMP socket. (Lesson #7.)
sudo setcap cap_net_raw+ep "$BIN_PATH"

# Verify the cap actually stuck (nosuid filesystem would silently drop it).
if ! sudo getcap "$BIN_PATH" | grep -q "cap_net_raw"; then
    echo -e "  ${RED}✗${RESET} setcap failed — restoring previous binary from backup" >&2
    sudo install -m 0755 -o "$USER" -g "$USER" "${BACKUP_DIR}/hoptrail" "$BIN_PATH"
    sudo setcap cap_net_raw+ep "$BIN_PATH" || true
    for svc in "${WAS_ACTIVE[@]}"; do sudo systemctl start "$svc" || true; done
    exit 1
fi
echo -e "  ${GREEN}✓${RESET} New binary in place with cap_net_raw+ep"

# Clear the staged file if we used one — a successful apply consumes it.
# Leaving it around would just confuse the next --staged run.
if [ "$STAGED" = true ]; then
    sudo rm -f "$STAGED_BIN"
fi

# --- [4/4] Start the services + verify ---
echo -e "${CYAN}[4/4]${RESET} Starting services..."
if [ "${#WAS_ACTIVE[@]}" -eq 0 ]; then
    echo -e "  ${YELLOW}·${RESET} No services were running before the update — none started"
fi
FAILED=()
for svc in "${WAS_ACTIVE[@]}"; do
    sudo systemctl start "$svc"
done
sleep 2
for svc in "${WAS_ACTIVE[@]}"; do
    if sudo systemctl is-active --quiet "$svc"; then
        echo -e "  ${GREEN}✓${RESET} ${svc} is running on new binary"
    else
        FAILED+=("$svc")
    fi
done

if [ "${#FAILED[@]}" -gt 0 ]; then
    # Best-effort rollback: restore the backed-up binary and restart
    # everything that was running. If THAT fails too, the operator has
    # the backup dir and the journal to dig into.
    echo -e "  ${RED}✗${RESET} Failed to start: ${FAILED[*]}. Attempting rollback..."
    sudo install -m 0755 -o "$USER" -g "$USER" "${BACKUP_DIR}/hoptrail" "$BIN_PATH"
    sudo setcap cap_net_raw+ep "$BIN_PATH"
    for svc in "${WAS_ACTIVE[@]}"; do
        sudo systemctl restart "$svc" || true
    done
    sleep 2
    STILL_DOWN=()
    for svc in "${WAS_ACTIVE[@]}"; do
        sudo systemctl is-active --quiet "$svc" || STILL_DOWN+=("$svc")
    done
    if [ "${#STILL_DOWN[@]}" -eq 0 ]; then
        echo -e "  ${YELLOW}!${RESET} Rolled back to previous binary. Services are running again."
        echo "    Check logs to see why the new binary failed:"
        for svc in "${FAILED[@]}"; do
            echo "      sudo journalctl -u ${svc} -n 50"
        done
    else
        echo -e "  ${RED}✗${RESET} Rollback also failed for: ${STILL_DOWN[*]}."
        echo "    Previous binary is at ${BACKUP_DIR}/hoptrail."
        for svc in "${STILL_DOWN[@]}"; do
            echo "    Check: sudo journalctl -u ${svc} -n 50"
        done
    fi
    exit 1
fi

echo ""
echo -e "${GREEN}══════════════════════════════════════════════${RESET}"
echo -e "${GREEN}  Update complete${RESET}"
echo -e "${GREEN}══════════════════════════════════════════════${RESET}"
echo ""
echo "  ${CURRENT_VERSION}  →  ${NEW_VERSION}"
echo ""
echo "  Previous binary backed up at: ${BACKUP_DIR}/hoptrail"
echo "  To roll back manually:"
echo "    sudo systemctl stop ${SERVICES[*]}"
echo "    sudo install -m 0755 ${BACKUP_DIR}/hoptrail ${BIN_PATH}"
echo "    sudo setcap cap_net_raw+ep ${BIN_PATH}"
echo "    sudo systemctl start ${SERVICES[*]}"
echo ""
