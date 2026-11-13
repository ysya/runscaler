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
        worker_hash=aaa126a2ed34483dab70b514539997399a65c19125d3e28414bec8d6b842eb48
        deps_hash=ae6528d6f947e829666b8b8cb4711a2ef460de9485c2ffd4e334b8324c140154
        listener_hash=5fcd3b762b1ef5c38c03692f886d9d525162e6b416424852de7a070e7c64b148
        ;;
    Linux/aarch64)
        platform=linux/arm64
        worker_hash=30c89622af0b7a60a315b7367f03cb076d514ebbc218f4d4697919fe4bc0706b
        deps_hash=f5c340f0374170bd15beff917fc452122dfe57653910b3c0cb2ae5ad91960a23
        listener_hash=bf9dd62247a9304bcb8d7e9dcc12b47fafab79e6ba36291ef21e0471c1000d94
        ;;
    *) fail 'only verified Linux amd64 and arm64 binaries are supported' ;;
esac

manifest() {
    cat <<MANIFEST
protocol=v2
platform=$platform
runner-version=2.338.2
runner-source=3efdc9c0d8a5b7a441cba3069b0fdc33d1931adb
runner-base=ghcr.io/falcondev-oss/actions-runner@sha256:462ebdbc95fabf0bdf264ffe02ff5182665c6d99ac0f870b67d7f649c1ef1746
runner-worker-patch-sha256=5c801dea0cbed7265cfa0e716d2f126ae1ed934c96000046cf89c89b701ccde0
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
