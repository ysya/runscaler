package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const probeDigest = "@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func runLocalCacheProbe(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	script, err := filepath.Abs("../../scripts/local-cache/probe.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("local cache probe is missing: %v", err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PROBE_DOCKER_LOG\"\nexit 91\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", append([]string{script}, args...)...)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "PROBE_DOCKER_LOG=" + log, "DOCKER_CONTEXT=ambient-production", "DOCKER_HOST=tcp://ambient.invalid:2375"}
	out, runErr := cmd.CombinedOutput()
	calls, readErr := os.ReadFile(log)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return string(out), string(calls), runErr
}

func TestLocalCacheProbeRequiresExplicitContext(t *testing.T) {
	for _, context := range []string{"", "default", "orbstack", "--help", "bad context"} {
		t.Run(context, func(t *testing.T) {
			args := []string{"--server-image", "ghcr.io/test/server" + probeDigest, "--runner-image", "ghcr.io/test/runner" + probeDigest}
			if context != "" {
				args = append(args, "--context", context)
			}
			out, calls, err := runLocalCacheProbe(t, args...)
			if err == nil || !strings.Contains(out, "explicit isolated Docker context") {
				t.Fatalf("output %q, err=%v; want context rejection", out, err)
			}
			if calls != "" {
				t.Fatalf("invalid input reached Docker: %s", calls)
			}
		})
	}
}

func TestLocalCacheProbeRejectsMutableImageRefs(t *testing.T) {
	refs := []string{"", "ghcr.io/test/server:latest", "ghcr.io/test/server:10.0.0", "ghcr.io/test/server@sha256:abc", "@sha256:" + strings.Repeat("a", 64), "--help" + probeDigest, "repo with space" + probeDigest, "ghcr.io/test/server@sha512:" + strings.Repeat("a", 128)}
	for _, field := range []string{"--server-image", "--runner-image"} {
		for _, ref := range refs {
			t.Run(field+"/"+ref, func(t *testing.T) {
				args := []string{"--context", "local-cache-isolated", "--server-image", "ghcr.io/test/server" + probeDigest, "--runner-image", "ghcr.io/test/runner" + probeDigest, field, ref}
				out, calls, err := runLocalCacheProbe(t, args...)
				if err == nil || !strings.Contains(out, "digest-qualified") {
					t.Fatalf("output %q, err=%v; want immutable image rejection", out, err)
				}
				if calls != "" {
					t.Fatalf("invalid input reached Docker: %s", calls)
				}
			})
		}
	}
}

func TestLocalCacheProbeUsesOnlyExplicitContext(t *testing.T) {
	_, calls, err := runLocalCacheProbe(t, "--context", "local-cache-isolated", "--server-image", "ghcr.io/test/server"+probeDigest, "--runner-image", "ghcr.io/test/runner"+probeDigest)
	if err == nil {
		t.Fatal("Docker failure must fail the probe")
	}
	if !strings.HasPrefix(calls, "--context local-cache-isolated ") {
		t.Fatalf("explicit context not forwarded: %q", calls)
	}
	if strings.Contains(calls, "ambient") {
		t.Fatalf("ambient Docker connection leaked: %q", calls)
	}
}

func TestLocalCacheProbeRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"--context"}, {"--unknown"}, {"--help"}} {
		out, calls, err := runLocalCacheProbe(t, args...)
		if calls != "" {
			t.Fatalf("invalid arguments reached Docker: %q", calls)
		}
		if args[0] == "--help" {
			if err != nil || !strings.Contains(out, "Usage:") {
				t.Fatalf("help output %q, err=%v", out, err)
			}
		} else if err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
}

func TestLocalCacheProbeCannotDeclareCompatibilityFromMetadata(t *testing.T) {
	script, err := filepath.Abs("../../scripts/local-cache/probe.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	stub := `#!/bin/sh
[ -z "${DOCKER_HOST+x}" ] && [ -z "${DOCKER_CONTEXT+x}" ] || exit 92
printf '%s\n' "$*" >> "$PROBE_DOCKER_LOG"
[ "$1" = --context ] && [ "$2" = local-cache-isolated ] || exit 93
shift 2
case "$1" in
 info) printf 'linux\n' ;;
 buildx) [ "$2" = imagetools ] && [ "$3" = inspect ] || exit 94 ;;
 *) exit 95 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", script, "--context", "local-cache-isolated", "--server-image", "ghcr.io/test/server"+probeDigest, "--runner-image", "ghcr.io/test/runner"+probeDigest)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "PROBE_DOCKER_LOG=" + log, "DOCKER_HOST=tcp://ambient.invalid:2375", "DOCKER_CONTEXT=ambient-production"}
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 3 || !strings.Contains(string(out), "compatibility_gate=incomplete") {
		t.Fatalf("metadata must not pass the full compatibility gate: output=%q, err=%v", out, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(calls)), "\n")); got != 3 {
		t.Fatalf("want daemon info and two read-only manifest requests, got %q", calls)
	}
}

func TestLocalCacheProbeSuiteValidation(t *testing.T) {
	for _, suite := range []string{"runtime", "clients", "lifecycle", "auth", "benchmark", "unknown"} {
		t.Run(suite, func(t *testing.T) {
			out, calls, err := runLocalCacheProbe(t, "--context", "local-cache-isolated", "--server-image", "test/server"+probeDigest, "--runner-image", "test/runner"+probeDigest, "--suite", suite)
			if err == nil {
				t.Fatal("stub Docker must fail")
			}
			if suite == "unknown" {
				if calls != "" || !strings.Contains(out, "unsupported suite") {
					t.Fatalf("invalid suite reached Docker: %q %q", out, calls)
				}
			} else if calls == "" {
				t.Fatalf("recognized suite rejected: %q", out)
			}
		})
	}
}

func runOwnedProbe(t *testing.T, stub string) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/bash\nset -eu\nprintf '%s\\n' \"$*\" >> \"$PROBE_DOCKER_LOG\"\n"+stub), 0700); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../scripts/local-cache/probe.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", script, "--context", "local-cache-isolated", "--server-image", "test/server"+probeDigest, "--runner-image", "test/runner"+probeDigest, "--execute", "--resource-prefix", "runscaler-probe-test")
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "PROBE_DOCKER_LOG=" + log}
	out, runErr := cmd.CombinedOutput()
	calls, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(out), string(calls), runErr
}

func TestLocalCacheProbeRefusesExistingResources(t *testing.T) {
	out, calls, err := runOwnedProbe(t, `shift 2
case "$1" in
 info) echo linux ;;
 buildx) : ;;
 ps) echo unowned-existing-container ;;
 *) exit 91 ;;
esac
`)
	if err == nil || !strings.Contains(out, "resource prefix already exists") {
		t.Fatalf("want collision refusal: out=%q calls=%q err=%v", out, calls, err)
	}
	for _, forbidden := range []string{" create ", " run ", " rm ", " prune", "delete"} {
		if strings.Contains(calls, forbidden) {
			t.Fatalf("collision caused mutation: %q", calls)
		}
	}
}

func TestLocalCacheProbeCleansOnlyRecordedOwnedContainer(t *testing.T) {
	out, calls, err := runOwnedProbe(t, `shift 2
case "$1" in
 info) echo linux ;;
 buildx|ps) : ;;
 network|volume) [ "$2" = ls ] || exit 91 ;;
 create) printf '%064d\n' 1 ;;
 start) exit 91 ;;
 inspect) echo runscaler-probe-test ;;
 rm) [ "$2" = -fv ] && [ "$3" = 0000000000000000000000000000000000000000000000000000000000000001 ] || exit 92 ;;
 *) exit 93 ;;
esac
`)
	if err == nil {
		t.Fatal("start failure must fail probe")
	}
	if !strings.Contains(calls, " rm -fv 0000000000000000000000000000000000000000000000000000000000000001") {
		t.Fatalf("owned resource not cleaned: out=%q calls=%q", out, calls)
	}
	if strings.Contains(calls, "prune") || strings.Contains(calls, "rm -fv runscaler") {
		t.Fatalf("unsafe deletion: %s", calls)
	}
}

func TestLocalCacheProbeSuccessfulRuntimeReturnsSuccess(t *testing.T) {
	out, calls, err := runOwnedProbe(t, `shift 2
case "$1" in
 info) echo linux ;;
 buildx|ps|start) : ;;
 network|volume) [ "$2" = ls ] || exit 91 ;;
 create) printf '%064d\n' 1 ;;
 inspect) [ "$3" = '{{index .Config.Labels "runscaler.cache-probe"}}' ] || exit 94; echo runscaler-probe-test ;;
 rm) [ "$2" = -fv ] && [ "$3" = 0000000000000000000000000000000000000000000000000000000000000001 ] || exit 92 ;;
 *) exit 93 ;;
esac
`)
	if err != nil || !strings.Contains(out, "runtime=pass") {
		t.Fatalf("runtime fixture failed: out=%q calls=%q err=%v", out, calls, err)
	}
	if !strings.Contains(calls, " rm -fv ") {
		t.Fatalf("owned runtime fixture leaked: %s", calls)
	}
}

func TestLocalCacheProbeRejectsUnverifiedRuntimeArchitecture(t *testing.T) {
	out, calls, err := runOwnedProbe(t, `shift 2
root=$(dirname "$PROBE_DOCKER_LOG")
case "$1" in
 info) echo linux ;;
 buildx|ps) : ;;
 network|volume) [ "$2" = ls ] || exit 91 ;;
 create)
   mkdir -p "$root/runner"
   printf '#!/bin/sh\necho unverified-runtime-listener-started\n' > "$root/runner/run.sh"
   printf '#!/bin/sh\nif [ "$1" = -s ]; then echo Linux; else echo aarch64; fi\n' > "$root/uname"
   # Simulate accepted assembly hashes to isolate the architecture precondition.
   printf '#!/bin/sh\ncat >/dev/null\nexit 0\n' > "$root/sha256sum"
   chmod +x "$root/runner/run.sh" "$root/uname" "$root/sha256sum"
   command="${!#}"
   command="${command//\/home\/runner/$root/runner}"
   printf '%s' "$command" > "$root/runtime.command"
   printf '%064d\n' 1 ;;
 start) /bin/bash -euc "$(cat "$root/runtime.command")" ;;
 inspect) echo runscaler-probe-test ;;
 rm) : ;;
 *) exit 93 ;;
esac
`)
	if err == nil || strings.Contains(out, "unverified-runtime-listener-started") {
		t.Fatalf("unverified architecture reached runner: out=%q err=%v", out, err)
	}
	if !strings.Contains(calls, " start -a ") {
		t.Fatalf("test did not execute the captured runtime command: %s", calls)
	}
}
