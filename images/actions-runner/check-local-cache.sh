#!/bin/sh
# This contract is tied to the actual binaries accepted by the compatibility gate.
set -eu

fail() { printf 'Local cache capability check failed: %s\n' "$1" >&2; exit 1; }
manifest_path=/usr/local/share/runscaler/local-actions-cache.manifest

manifest() {
    cat <<'MANIFEST'
protocol=v2
platform=linux/amd64
runner-version=2.338.2
runner-source=3efdc9c0d8a5b7a441cba3069b0fdc33d1931adb
runner-base=ghcr.io/falcondev-oss/actions-runner@sha256:462ebdbc95fabf0bdf264ffe02ff5182665c6d99ac0f870b67d7f649c1ef1746
worker-sha256=5516e3a6b4cdebef7ee9381790bb5929d8c699b6cf33f11dd93746822a796188
listener-sha256=5fcd3b762b1ef5c38c03692f886d9d525162e6b416424852de7a070e7c64b148
MANIFEST
}

check_file() {
    [ -r "$1" ] || fail 'a verified runner assembly is missing'
    actual_hash=$(sha256sum "$1") || fail 'cannot checksum runner assembly'
    [ "${actual_hash%% *}" = "$2" ] || fail 'runner assembly does not match the accepted checksum'
}

check_binaries() {
    [ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] || fail 'only verified Linux amd64 binaries are supported'
    check_file /home/runner/bin/Runner.Worker.dll 5516e3a6b4cdebef7ee9381790bb5929d8c699b6cf33f11dd93746822a796188
    check_file /home/runner/bin/Runner.Listener.dll 5fcd3b762b1ef5c38c03692f886d9d525162e6b416424852de7a070e7c64b148
}

# Build-only emission validates known binaries, rather than blessing whatever
# hashes happen to be present. The provider never invokes this mode.
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
# No network request: an unavailable cache service must not turn this binary
# capability check into a mandatory server-health dependency.
