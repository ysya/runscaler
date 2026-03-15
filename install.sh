#!/bin/sh
set -eu

REPO="ysya/runscaler"
BINARY="runscaler"
INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"

fail() { echo "Error: $1" >&2; exit 1; }

# Detect OS
case "$(uname -s)" in
    Linux*)  OS="linux" ;;
    Darwin*) OS="darwin" ;;
    *)       fail "Unsupported OS: $(uname -s). Use 'go install github.com/${REPO}@latest' instead." ;;
esac

# Detect architecture
case "$(uname -m)" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)             fail "Unsupported architecture: $(uname -m)" ;;
esac

# Pick download tool
if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL "$1"; }
    download() { curl -fsSL -o "$1" "$2"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -qO- "$1"; }
    download() { wget -qO "$1" "$2"; }
else
    fail "curl or wget is required"
fi

# Resolve version
if [ -z "${RUNSCALER_VERSION:-}" ]; then
    RUNSCALER_VERSION=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//')
    [ -n "$RUNSCALER_VERSION" ] || fail "Could not determine latest version"
fi

ARCHIVE="${BINARY}-${OS}-${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${RUNSCALER_VERSION}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading ${BINARY} ${RUNSCALER_VERSION} (${OS}/${ARCH})..."
download "${TMPDIR}/${ARCHIVE}" "${BASE_URL}/${ARCHIVE}"
download "${TMPDIR}/checksums.txt" "${BASE_URL}/checksums.txt"

# Verify checksum
cd "$TMPDIR"
EXPECTED=$(grep "${ARCHIVE}" checksums.txt | awk '{print $1}')
[ -n "$EXPECTED" ] || fail "Checksum not found for ${ARCHIVE}"

if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL=$(sha256sum "${ARCHIVE}" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
    ACTUAL=$(shasum -a 256 "${ARCHIVE}" | awk '{print $1}')
else
    fail "sha256sum or shasum is required for checksum verification"
fi

[ "$EXPECTED" = "$ACTUAL" ] || fail "Checksum mismatch: expected ${EXPECTED}, got ${ACTUAL}"

# Extract and install
tar xzf "${ARCHIVE}"
install -d "${INSTALL_DIR}"
install "${BINARY}" "${INSTALL_DIR}/${BINARY}"

echo "Installed ${BINARY} ${RUNSCALER_VERSION} to ${INSTALL_DIR}/${BINARY}"

# Warn if INSTALL_DIR is not in PATH
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
        echo ""
        echo "NOTE: ${INSTALL_DIR} is not in your PATH. Add this to your shell profile:"
        echo "  export PATH=\"\${HOME}/.local/bin:\${PATH}\""
        ;;
esac
