#!/bin/sh
# This contract is tied to the runner binaries and source patch accepted by the gate.
set -eu

fail() { printf 'Local cache capability check failed: %s\n' "$1" >&2; exit 1; }
manifest_path=/usr/local/share/runscaler/local-actions-cache.manifest

manifest() {
    cat <<'MANIFEST'
protocol=v2
platform=linux/amd64
runner-version=2.338.2
runner-source=3efdc9c0d8a5b7a441cba3069b0fdc33d1931adb
runner-worker-patch-sha256=5c801dea0cbed7265cfa0e716d2f126ae1ed934c96000046cf89c89b701ccde0
runner-parent-image=ghcr.io/ysya/actions-runner-local-cache@sha256:ce94d981fbd73f15788bf98af0055d8ccea29a50932484e50c962c67cd45d577
worker-sha256=e7a2751d8a55743583582457c4ac746b09f4a0401a891327acede6ea9d7c14e5
listener-sha256=5fcd3b762b1ef5c38c03692f886d9d525162e6b416424852de7a070e7c64b148
legacy-v1=local-reject
MANIFEST
}

check_file() {
    [ -r "$1" ] || fail 'a verified runner assembly is missing'
    actual_hash=$(sha256sum "$1") || fail 'cannot checksum runner assembly'
    [ "${actual_hash%% *}" = "$2" ] || fail 'runner assembly does not match the accepted checksum'
}

check_binaries() {
    [ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] || fail 'only verified Linux amd64 binaries are supported'
    check_file /home/runner/bin/Runner.Worker.dll e7a2751d8a55743583582457c4ac746b09f4a0401a891327acede6ea9d7c14e5
    check_file /home/runner/bin/Runner.Listener.dll 5fcd3b762b1ef5c38c03692f886d9d525162e6b416424852de7a070e7c64b148
}

if [ "$#" -eq 1 ] && [ "$1" = --emit-manifest ]; then
    check_binaries
    manifest
    exit 0
fi
[ "$#" -eq 0 ] || fail 'unexpected argument'
[ -n "${CUSTOM_ACTIONS_RESULTS_URL:-}" ] || fail 'local cache endpoint is not set'
[ -r "$manifest_path" ] || fail 'verified capability manifest is missing'
[ "$(cat "$manifest_path")" = "$(manifest)" ] || fail 'capability manifest does not match the accepted contract'
check_binaries
