#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  echo "Please run as root" >&2
  exit 1
fi

AGENT_ID="${AGENT_ID:-}"
AGENT_TOKEN="${AGENT_TOKEN:-}"
AGENT_SERVER_URL="${AGENT_SERVER_URL:-}"
AGENT_HTTP_BASE="${AGENT_HTTP_BASE:-}"
AGENT_SHELL="${AGENT_SHELL:-/bin/bash}"
AGENT_WORKDIR="${AGENT_WORKDIR:-.}"

BIN_PATH="/usr/local/bin/terminal-bridge-agent"
SERVICE_PATH="/etc/systemd/system/terminal-bridge-agent.service"
ENV_PATH="/etc/terminal-bridge-agent.env"

if [[ -z "${AGENT_ID}" || -z "${AGENT_TOKEN}" || -z "${AGENT_SERVER_URL}" ]]; then
  echo "Missing required envs: AGENT_ID, AGENT_TOKEN, AGENT_SERVER_URL" >&2
  echo "Example:" >&2
  echo "  AGENT_ID=home-gw AGENT_TOKEN=xxx AGENT_SERVER_URL=wss://your-domain/agent/ws AGENT_HTTP_BASE=https://your-domain bash install-agent.sh" >&2
  exit 1
fi

if [[ -z "${AGENT_HTTP_BASE}" ]]; then
  AGENT_HTTP_BASE="${AGENT_SERVER_URL}"
  AGENT_HTTP_BASE="${AGENT_HTTP_BASE%%/agent/ws*}"
  AGENT_HTTP_BASE="${AGENT_HTTP_BASE/ws:\/\//http:\/\/}"
  AGENT_HTTP_BASE="${AGENT_HTTP_BASE/wss:\/\//https:\/\/}"
fi

ARCH_RAW="$(uname -m)"
case "${ARCH_RAW}" in
  x86_64|amd64)
    AGENT_ARCH="amd64"
    ;;
  aarch64|arm64)
    AGENT_ARCH="arm64"
    ;;
  *)
    echo "Unsupported architecture: ${ARCH_RAW}" >&2
    exit 1
    ;;
esac

DOWNLOAD_URL="${AGENT_HTTP_BASE%/}/agent/download?os=linux&arch=${AGENT_ARCH}"

echo "Downloading agent binary from ${DOWNLOAD_URL} ..."
curl -fsSL "${DOWNLOAD_URL}" -o "${BIN_PATH}"
chmod +x "${BIN_PATH}"

cat >"${ENV_PATH}" <<EOF
AGENT_SERVER_URL=${AGENT_SERVER_URL}
AGENT_ID=${AGENT_ID}
AGENT_TOKEN=${AGENT_TOKEN}
AGENT_SHELL=${AGENT_SHELL}
AGENT_WORKDIR=${AGENT_WORKDIR}
EOF

chmod 600 "${ENV_PATH}"

cat >"${SERVICE_PATH}" <<EOF
[Unit]
Description=terminal-bridge gateway agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${ENV_PATH}
ExecStart=${BIN_PATH}
Restart=always
RestartSec=3
User=root
WorkingDirectory=/

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now terminal-bridge-agent

echo "Installed. Service status:"
systemctl --no-pager --full status terminal-bridge-agent || true
