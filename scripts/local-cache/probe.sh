#!/usr/bin/env bash
# Isolated fixture runner. Preflight alone is not a signed-job compatibility verdict.
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: scripts/local-cache/probe.sh --context <isolated-context> \
    --server-image <repository@sha256:digest> --runner-image <repository@sha256:digest>
    [--suite runtime|clients|lifecycle|auth|benchmark] [--execute --resource-prefix runscaler-probe-<id>]

Use a separately provisioned, isolated Linux Docker daemon. The context name
alone does not establish isolation. Without --execute this only reads metadata.
--execute authorizes bounded disposable fixtures and cleanup of their recorded,
label-verified IDs. The prefix must be unused. No blanket prune is performed.
clients requires real signed GitHub jobs using the manual acceptance workflow;
this CLI never registers runners or treats metadata as client evidence.
Exit codes: 0 help/selected suite passed, 2 invalid input, 3 full gate incomplete.
A selected suite pass is never a full signed-job compatibility verdict.
Docker or registry failures also exit nonzero.
USAGE
}

fail() { printf 'Error: %s\n' "$1" >&2; exit 2; }
context=''
server_image=''
runner_image=''
suite=runtime
execute=false
prefix=''
while (( $# )); do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --execute) execute=true; shift ;;
        --context|--server-image|--runner-image|--suite|--resource-prefix)
            (( $# >= 2 )) || fail "missing option value"
            case "$1" in
                --context) context=$2 ;;
                --server-image) server_image=$2 ;;
                --runner-image) runner_image=$2 ;;
                --suite) suite=$2 ;;
                --resource-prefix) prefix=$2 ;;
            esac
            shift 2 ;;
        *) fail "unknown argument; use --help" ;;
    esac
done

case "$suite" in runtime|clients|lifecycle|auth|benchmark) ;; *) fail 'unsupported suite' ;; esac
if $execute; then
    [[ $prefix =~ ^runscaler-probe-[a-z0-9][a-z0-9-]{0,36}$ ]] || fail 'an explicit runscaler-probe- resource prefix is required'
fi

# Validate every input before invoking Docker. Do not echo possibly sensitive input.
[[ $context =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || fail "an explicit isolated Docker context is required"
case "$context" in
    default|orbstack|desktop-linux|desktop-windows)
        fail "an explicit isolated Docker context is required; shared contexts are refused" ;;
esac
valid_image() {
    [[ $1 =~ ^[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}$ ]]
}
valid_image "$server_image" || fail "server image must be digest-qualified (repository@sha256:<64 lowercase hex>)"
valid_image "$runner_image" || fail "runner image must be digest-qualified (repository@sha256:<64 lowercase hex>)"

# The explicit context must win over ambient DOCKER_HOST/DOCKER_CONTEXT settings.
unset DOCKER_HOST DOCKER_CONTEXT
engine_os=$(docker --context "$context" info --format '{{.OSType}}')
[[ $engine_os == linux ]] || fail "the isolated Docker daemon must run Linux"
printf 'preflight_context=%s\n' "$context"
printf 'preflight_server_image=%s\n' "$server_image"
printf 'preflight_runner_image=%s\n' "$runner_image"
for image in "$server_image" "$runner_image"; do
    docker --context "$context" buildx imagetools inspect "$image"
done

if $execute; then
    if [[ $suite == clients ]]; then
        printf '%s\n' 'clients=unknown; run .github/workflows/local-cache-acceptance.yml with fresh dedicated runners' >&2
        exit 3
    fi
    d() { docker --context "$context" "$@"; }
    # Refuse collisions even when a pre-existing object has a matching owner label.
    [[ -z $(d ps -aq --filter "name=^/${prefix}(-|$)") ]] || fail 'resource prefix already exists'
    [[ -z $(d network ls -q --filter "name=^${prefix}(-|$)") ]] || fail 'resource prefix already exists'
    [[ -z $(d volume ls -q --filter "name=^${prefix}(-|$)") ]] || fail 'resource prefix already exists'
    owner=runscaler.cache-probe
    container_owner_format='{{index .Config.Labels "runscaler.cache-probe"}}'
    resource_owner_format='{{index .Labels "runscaler.cache-probe"}}'
    containers=()
    networks=()
    volumes=()
    cleanup() {
        local id
        for id in "${containers[@]:-}"; do
            [[ -n $id ]] || continue
            if [[ $(d inspect --format "$container_owner_format" "$id" 2>/dev/null) == "$prefix" ]]; then d rm -fv "$id" >/dev/null || true; fi
        done
        for id in "${networks[@]:-}"; do
            [[ -n $id ]] || continue
            if [[ $(d network inspect --format "$resource_owner_format" "$id" 2>/dev/null) == "$prefix" ]]; then d network rm "$id" >/dev/null || true; fi
        done
        for id in "${volumes[@]:-}"; do
            [[ -n $id ]] || continue
            if [[ $(d volume inspect --format "$resource_owner_format" "$id" 2>/dev/null) == "$prefix" ]]; then d volume rm "$id" >/dev/null || true; fi
        done
    }
    trap cleanup EXIT
    create_container() {
        local id
        id=$(d create "$@")
        [[ $id =~ ^[a-f0-9]{64}$ ]] || fail 'Docker returned an invalid container ID'
        containers+=("$id")
        created=$id
    }
    if [[ $suite == runtime ]]; then
        create_container --name "$prefix-runtime" --label "$owner=$prefix" --network none --cpus 1 --memory 512m --pids-limit 128 --entrypoint /bin/bash "$runner_image" -euc '
          [ "$(uname -s)/$(uname -m)" = Linux/x86_64 ] || { echo "Unsupported local cache runtime platform" >&2; exit 1; }
          cd /home/runner
          printf "%s  %s\n" 5516e3a6b4cdebef7ee9381790bb5929d8c699b6cf33f11dd93746822a796188 bin/Runner.Worker.dll 5fcd3b762b1ef5c38c03692f886d9d525162e6b416424852de7a070e7c64b148 bin/Runner.Listener.dll | sha256sum --check --status
          ./run.sh --version
        '
        d start -a "$created"
        printf '%s\n' 'runtime=pass (known amd64 assemblies/version only); signed_jobs=unknown'
        exit 0
    fi
    network=$(d network create --label "$owner=$prefix" "$prefix-network"); networks+=("$network")
    volume=$(d volume create --label "$owner=$prefix" "$prefix-data"); volumes+=("$volume")
    base="http://$prefix-server:3000/"
    issuer="http://$prefix-client:4080"
    create_container --name "$prefix-client" --label "$owner=$prefix" --network "$network" --cpus 0.5 --memory 256m --pids-limit 64 -e "PROBE_CACHE_URL=$base" -e "PROBE_ISSUER_URL=$issuer" --entrypoint /home/runner/externals/node24/bin/node "$runner_image" /tmp/cache-protocol-fixture.mjs
    client=$created
    d cp "$(dirname "$0")/cache-protocol-fixture.mjs" "$client:/tmp/cache-protocol-fixture.mjs"
    d start "$client" >/dev/null
    start_server() {
        create_container --name "$prefix-server" --label "$owner=$prefix" --network "$network" --cpus 0.5 --memory 256m --pids-limit 128 -e "API_BASE_URL=$base" -e STORAGE_DRIVER=filesystem -e DB_DRIVER=sqlite -e SKIP_TOKEN_VALIDATION=false -e CACHE_MAX_SIZE_BYTES=67108864 -e "ACTIONS_TOKEN_ISSUER=$issuer" -e "ACTIONS_TOKEN_JWKS_URL=$issuer/jwks" -e ACTIONS_TOKEN_AUDIENCE=runscaler-gate -e "DEFAULT_ACTIONS_RESULTS_URL=$issuer" -v "$volume:/app/.data" "$server_image"
        server=$created
        d start "$server" >/dev/null
        d exec "$client" /home/runner/externals/node24/bin/node -e 'const u=process.env.PROBE_CACHE_URL; (async()=>{for(let i=0;i<50;i++){try{if((await fetch(new URL("health",u))).ok)return}catch{}await new Promise(r=>setTimeout(r,100))}process.exitCode=1})()'
    }
    fixture() {
        d exec "$client" /home/runner/externals/node24/bin/node -e 'fetch("http://localhost:4080/run",{method:"POST",body:JSON.stringify({phase:process.argv[1]})}).then(async r=>{const t=await r.text();if(!r.ok)throw Error(t);console.log(t)}).catch(e=>{console.error(e.message);process.exitCode=1})' "$1"
    }
    start_server
    fixture roundtrip
    fixture cancellation
    case "$suite" in
        lifecycle)
            d stop "$server" >/dev/null
            [[ $(d inspect --format "$container_owner_format" "$server") == "$prefix" ]] || fail 'server ownership changed'
            d rm "$server" >/dev/null
            start_server
            fixture restore
            fixture cancellation-restore ;;
        benchmark)
            # Protocol timings are not language build/client benchmarks.
            for pair in 1 2 3; do
                printf 'protocol_pair=%s\n' "$pair"
                fixture roundtrip
                fixture restore
            done ;;
    esac
    printf 'suite=%s result=pass scope=controlled-protocol signed_jobs=unknown\n' "$suite"
    exit 0
fi

# Registry metadata cannot prove actual binaries, JIT, handlers, authentication,
# artifact passthrough, or absence of cloud cache writes. Never claim a pass here.
printf '%s\n' 'compatibility_gate=incomplete' >&2
printf '%s\n' 'Signed-job, binary provenance, and isolated lifecycle evidence are required; see docs/local-actions-cache-compatibility.md.' >&2
exit 3
