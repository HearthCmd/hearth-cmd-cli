#!/usr/bin/env bash
set -euo pipefail

REPO="HearthCmd/hearth-cmd-cli"
INSTALL_DIR="${HOME}/.local/bin"
BINARY_NAME="hearth"

# --- Detect OS and architecture ---
OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
  Darwin) os="darwin" ;;
  Linux)  os="linux" ;;
  *)
    echo "Error: Unsupported operating system: $OS" >&2
    echo "hearth CLI supports macOS and Linux only." >&2
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64)  arch="amd64" ;;
  aarch64) arch="arm64" ;;
  arm64)   arch="arm64" ;;
  *)
    echo "Error: Unsupported architecture: $ARCH" >&2
    echo "hearth CLI supports amd64 and arm64 only." >&2
    exit 1
    ;;
esac

ASSET_NAME="hearth-${os}-${arch}"

# --- Resolve latest release tag ---
echo "Fetching latest release..."
if command -v curl &>/dev/null; then
  LATEST_TAG=$(curl -sI "https://github.com/${REPO}/releases/latest" \
    | grep -i '^location:' \
    | sed 's/.*tag\///' \
    | tr -d '\r\n')
elif command -v wget &>/dev/null; then
  LATEST_TAG=$(wget --spider --server-response "https://github.com/${REPO}/releases/latest" 2>&1 \
    | grep -i 'location:' \
    | tail -1 \
    | sed 's/.*tag\///' \
    | tr -d '\r\n')
else
  echo "Error: curl or wget is required." >&2
  exit 1
fi

if [ -z "$LATEST_TAG" ]; then
  echo "Error: Could not determine latest release." >&2
  exit 1
fi

DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST_TAG}/${ASSET_NAME}"
echo "Latest release: ${LATEST_TAG}"
echo "Downloading ${ASSET_NAME}..."

# --- Download binary ---
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

if command -v curl &>/dev/null; then
  curl -fSL --progress-bar -o "${tmpdir}/${BINARY_NAME}" "$DOWNLOAD_URL"
elif command -v wget &>/dev/null; then
  wget -q --show-progress -O "${tmpdir}/${BINARY_NAME}" "$DOWNLOAD_URL"
fi

chmod +x "${tmpdir}/${BINARY_NAME}"

# --- Verify download ---
if [ ! -s "${tmpdir}/${BINARY_NAME}" ]; then
  echo "Error: Download failed or file is empty." >&2
  exit 1
fi

# --- Install ---
mkdir -p "$INSTALL_DIR"
echo "Installing to ${INSTALL_DIR}/${BINARY_NAME}..."
mv "${tmpdir}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"

echo ""
echo "Done. Run 'hearth' to get started."
echo "Make sure ${INSTALL_DIR} is on your PATH:"
echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
