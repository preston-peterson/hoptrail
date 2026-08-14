#!/bin/bash
# =============================================================================
# Hoptrail — local ntfy server install helper
# =============================================================================
#
# Installs a local ntfy server (https://ntfy.sh) for hoptrail's alert
# delivery. Runs as ROOT via the /etc/sudoers.d/hoptrail rule — this
# is what the "install a local ntfy" button in the Alerts settings
# executes. Plain-text output (shown verbatim in the UI on failure).
#
# Refuses when an ntfy is already present (binary or systemd unit):
# two ntfy daemons fighting over a port helps nobody — point hoptrail
# at the existing one instead (Alerts settings → ntfy server URL).
#
# What it does:
#   - Downloads the pinned ntfy release for this CPU, verifies the
#     SHA256, installs to /usr/local/bin/ntfy
#   - Writes /etc/ntfy/server.yml (listens on :2586 — the phone app
#     must reach it over the LAN; no auth, same trusted-network
#     stance as hoptrail's own UI)
#   - Installs + starts a hardened systemd unit (DynamicUser)
#   - Stamps /etc/ntfy/hoptrail-installed so uninstall.sh knows this
#     ntfy is hoptrail-managed (an operator's own ntfy is never touched)
#
# Exit codes: 0 installed · 1 failed · 3 ntfy already present
# =============================================================================

NTFY_VERSION="2.24.0"
SHA256_AMD64="4789b38c1c068ef849f95645df4dcb100a7a05f94b29b3cff85153ff4d3b29bb"
SHA256_ARM64="42041d3587bea2df3bbff65fb88cc273af32c1668b625a7ea3318230bf064739"
NTFY_PORT="2586"

if [ "$(id -u)" -ne 0 ]; then
    echo "error: must run as root (via sudo)" >&2
    exit 1
fi

if command -v ntfy >/dev/null 2>&1; then
    echo "An ntfy is already installed at $(command -v ntfy)." >&2
    echo "Point hoptrail at it instead: Alerts settings -> ntfy server URL." >&2
    exit 3
fi
if [ -f /etc/systemd/system/ntfy.service ] || systemctl list-unit-files 2>/dev/null | grep -q "^ntfy.service"; then
    echo "An ntfy systemd unit already exists on this host." >&2
    echo "Point hoptrail at it instead: Alerts settings -> ntfy server URL." >&2
    exit 3
fi
if ! command -v curl >/dev/null 2>&1; then
    echo "error: curl is required" >&2
    exit 1
fi

case "$(uname -m)" in
    x86_64)  ARCH="amd64"; SHA256="$SHA256_AMD64" ;;
    aarch64) ARCH="arm64"; SHA256="$SHA256_ARM64" ;;
    *) echo "error: unsupported architecture $(uname -m) — install ntfy manually: https://docs.ntfy.sh/install/" >&2; exit 1 ;;
esac

TARBALL="ntfy_${NTFY_VERSION}_linux_${ARCH}.tar.gz"
URL="https://github.com/binwiederhier/ntfy/releases/download/v${NTFY_VERSION}/${TARBALL}"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading ntfy v${NTFY_VERSION} (${ARCH})..."
if ! curl -fsSL -o "${TMPDIR}/${TARBALL}" "$URL"; then
    echo "error: download failed: $URL" >&2
    exit 1
fi
echo "${SHA256}  ${TMPDIR}/${TARBALL}" | sha256sum -c --quiet - || {
    echo "error: checksum mismatch — refusing to install" >&2
    exit 1
}
tar -xzf "${TMPDIR}/${TARBALL}" -C "$TMPDIR" || { echo "error: extract failed" >&2; exit 1; }
BIN="$(find "$TMPDIR" -type f -name ntfy | head -1)"
if [ -z "$BIN" ]; then
    echo "error: ntfy binary not found in the release archive" >&2
    exit 1
fi
install -m 0755 -o root -g root "$BIN" /usr/local/bin/ntfy

install -d -m 0755 /etc/ntfy
cat > /etc/ntfy/server.yml <<EOF
# Written by hoptrail's install-ntfy helper. Listens on the LAN so the
# ntfy phone app can subscribe directly; no auth — trusted-network
# deployment, same stance as hoptrail's own web UI.
listen-http: ":${NTFY_PORT}"
cache-file: "/var/cache/ntfy/cache.db"
EOF

cat > /etc/systemd/system/ntfy.service <<'EOF'
[Unit]
Description=ntfy push notification server (installed by hoptrail)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/ntfy serve --config /etc/ntfy/server.yml
Restart=on-failure
RestartSec=5
DynamicUser=yes
CacheDirectory=ntfy
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
EOF

touch /etc/ntfy/hoptrail-installed
systemctl daemon-reload
systemctl enable --now ntfy || { echo "error: ntfy service failed to start; journalctl -u ntfy" >&2; exit 1; }

# Health check — give it a moment to bind.
sleep 2
if curl -fsS "http://127.0.0.1:${NTFY_PORT}/v1/health" >/dev/null 2>&1; then
    echo "ntfy v${NTFY_VERSION} installed and healthy on port ${NTFY_PORT}"
    exit 0
fi
echo "ntfy installed but the health check did not answer yet — check: journalctl -u ntfy" >&2
exit 1
