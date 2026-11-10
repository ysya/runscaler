#!/bin/sh
# Destructive fixture tests: execute only inside a disposable image-contract container.
set -eu
[ "${RUNSCALER_LOCAL_CACHE_CHECKER_FIXTURE:-}" = 1 ] && [ -f /.dockerenv ] && [ "$(id -u)" = 0 ] || {
    echo 'Refusing to modify assemblies outside an explicitly enabled disposable container fixture' >&2
    exit 2
}
fixture_dir=$(mktemp -d /tmp/runscaler-cache-check.XXXXXX)
manifest=/usr/local/share/runscaler/local-actions-cache.manifest
worker=/home/runner/bin/Runner.Worker.dll
listener=/home/runner/bin/Runner.Listener.dll
run=/home/runner/run.sh
marker=/tmp/runscaler-cache-listener-started
cp "$manifest" "$fixture_dir/manifest"
cp "$worker" "$fixture_dir/worker"
cp "$listener" "$fixture_dir/listener"
cp "$run" "$fixture_dir/run"
restore() {
    cp "$fixture_dir/manifest" "$manifest"
    cp "$fixture_dir/worker" "$worker"
    cp "$fixture_dir/listener" "$listener"
    cp "$fixture_dir/run" "$run"
    rm -f "$marker"
    rm -rf "$fixture_dir"
}
trap restore EXIT
cat > "$run" <<'LISTENER'
#!/bin/sh
printf 'started' > /tmp/runscaler-cache-listener-started
LISTENER
chmod 0755 "$run"
reject() {
    rm -f "$marker"
    if sh -c '/usr/local/bin/runner-local-cache-check && exec /home/runner/run.sh' > "$fixture_dir/output" 2>&1; then
        echo "checker unexpectedly accepted: $1" >&2; exit 1
    fi
    [ ! -e "$marker" ] || { echo "listener executed after rejected check: $1" >&2; exit 1; }
    printf '%s=rejected-before-listener\n' "$1"
}
unset CUSTOM_ACTIONS_RESULTS_URL
reject missing-endpoint
export CUSTOM_ACTIONS_RESULTS_URL=http://offline-cache.invalid:9/
mv "$manifest" "$fixture_dir/hidden-manifest"
reject missing-manifest
mv "$fixture_dir/hidden-manifest" "$manifest"
printf 'protocol=v1\n' > "$manifest"
reject wrong-contract
cp "$fixture_dir/manifest" "$manifest"
grep -v '^worker-sha256=' "$fixture_dir/manifest" > "$manifest"
reject missing-checksum
cp "$fixture_dir/manifest" "$manifest"
printf 'modified' >> "$worker"
reject modified-worker
cp "$fixture_dir/worker" "$worker"
printf 'modified' >> "$listener"
reject modified-listener
cp "$fixture_dir/listener" "$listener"
sh -c '/usr/local/bin/runner-local-cache-check && exec /home/runner/run.sh'
[ -s "$marker" ] || { echo 'verified offline contract did not admit listener' >&2; exit 1; }
printf 'verified-offline-contract=accepted\n'
