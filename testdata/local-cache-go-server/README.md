# Isolated Go cache receiver

This test-only binary starts the same `actionscache.Service` used by the manager,
without starting scale-set controllers. Use it only inside an isolated Linux
Docker daemon with a fresh data directory and normal GitHub JWT verification.
It defaults to GitHub's configured issuer, JWKS endpoint, and Results upstream.

Build for the first accepted architecture:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -o cache-fixture ./testdata/local-cache-go-server
```

Run inside the isolated network with a private listener IP that both the
temporary runner and BuildKit can reach:

```sh
RUNSCALER_NATIVE_CACHE_FIXTURE=1 ./cache-fixture \
  --listen-address 172.17.0.1:3001 \
  --advertise-url http://172.17.0.1:3001/ \
  --data-dir /tmp/native-cache-v2
```

The listener IP above is an example. Verify the actual bridge address before
starting. Never mount the production Rust volume, production manager directory,
or Docker socket into this fixture. The program exits on SIGINT/SIGTERM and
shuts the receiver down gracefully. It does not register runners or dispatch
GitHub workflows; those steps require separately approved canary resources.
