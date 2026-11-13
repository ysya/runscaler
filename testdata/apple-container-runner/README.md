# Apple Container runner image

This is a reproducible arm64 image recipe for the Apple Container provider. Build it with a pinned Actions runner release and its published SHA-256 value:

```sh
docker build --platform linux/arm64 \
  --build-arg ACTIONS_RUNNER_VERSION=2.338.2 \
  --build-arg ACTIONS_RUNNER_SHA256=670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945 \
  -t example.invalid/actions-runner:arm64 .
```

The image must include the Actions runner at `/opt/actions-runner` and expose `/usr/local/bin/apple-runner` as its entrypoint. The provider passes the GitHub JIT configuration through `ACTIONS_RUNNER_INPUT_JITCONFIG`. A real GitHub App registration is required for a job-level smoke test.
