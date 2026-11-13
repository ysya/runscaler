#!/bin/sh
# Verify the reviewed runner assemblies before a local-cache job starts.
set -eu

fail() { printf 'Local cache capability check failed: %s\n' "$1" >&2; exit 1; }
manifest_path=/usr/local/share/runscaler/local-actions-cache.manifest

if [ "$#" -eq 0 ]; then
    [ -n "${CUSTOM_ACTIONS_RESULTS_URL:-}" ] || fail 'local cache endpoint is not set'
elif [ "$#" -ne 1 ] || [ "$1" != --emit-manifest ]; then
    fail 'unexpected argument'
fi

case "$(uname -s)/$(uname -m)" in
    Linux/x86_64)
        platform=linux/amd64
        worker_hash=301a0da6e1423176812111b207daa567e00d4995b8f9379861c8b84c20ddcf23
        deps_hash=4e394d56562dcffc30b0a5dcf5a4a8b5491f4c8836ceb7b40b81613f6258d38c
        listener_hash=66d3713bf1ece5a902c24a64667c93d6f4e8d0c31935820995beadad6b423761
        ;;
    Linux/aarch64)
        platform=linux/arm64
        worker_hash=72aa438dc11905d63bcf8970ec25b4d36e305b05a722b3d01c0786fcc2b11db3
        deps_hash=5ea5e6f70f549b7881ad584c7d0839117d97640a07ee6e2cd6e9769c60d42f9c
        listener_hash=27d84ac75baf30c4cbf402cfecce9336f40ef35c9f4d529e2e8ddf3a279a950b
        ;;
    *) fail 'only verified Linux amd64 and arm64 binaries are supported' ;;
esac

manifest() {
    cat <<MANIFEST
protocol=v2
platform=$platform
runner-version=2.338.2
runner-source=d1eb12d4b115c301201de27d667284e1a80f2ab5
runner-base=ghcr.io/actions/actions-runner@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300
runner-local-cache-patch-sha256=04fb2e9321520b29c3a2399370cfa64fbeef9ef58e3d5f770a357f56021583b2
runner-audit-patch-sha256=f258207452fd3a3106d9fa0e2cac397ba76ea8c40e88206c84ff3ad8ff2013cc
worker-sha256=$worker_hash
worker-deps-sha256=$deps_hash
webutilities-sha256=e13d467a95c936bf1ad4af66c3203682e3f555a2734f4a302d9b95a7971e19da
listener-sha256=$listener_hash
legacy-v1=local-reject
MANIFEST
}

check_file() {
    [ -r "$1" ] || fail 'a verified runner assembly is missing'
    actual_hash=$(sha256sum "$1") || fail 'cannot checksum runner assembly'
    [ "${actual_hash%% *}" = "$2" ] || fail 'runner assembly does not match the accepted checksum'
}

check_binaries() {
    check_file /home/runner/bin/Runner.Worker.dll "$worker_hash"
    check_file /home/runner/bin/Runner.Worker.deps.json "$deps_hash"
    check_file /home/runner/bin/Microsoft.AspNetCore.WebUtilities.dll e13d467a95c936bf1ad4af66c3203682e3f555a2734f4a302d9b95a7971e19da
    check_file /home/runner/bin/Runner.Listener.dll "$listener_hash"
}

if [ "$#" -eq 1 ] && [ "$1" = --emit-manifest ]; then
    check_binaries
    manifest
    exit 0
fi
[ -r "$manifest_path" ] || fail 'verified capability manifest is missing'
[ "$(cat "$manifest_path")" = "$(manifest)" ] || fail 'capability manifest does not match the accepted contract'
check_binaries
