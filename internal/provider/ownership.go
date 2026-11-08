package provider

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Runner names every container and VM it creates by one of three schemes and
// recognises its own leftovers only by those exact schemes. Startup
// reconciliation and `runner doctor --fix` delete whatever they recognise, so
// a resource another tool happened to call "runner-something" must never
// match.

const (
	// ManagedByLabel and ManagedByValue tag every container runner creates;
	// the label is the primary ownership signal for containers.
	ManagedByLabel = "managed-by"
	ManagedByValue = "runner"
)

var (
	// runnerNamePattern matches NewRunnerName: eight hex digits of a UUID.
	runnerNamePattern = regexp.MustCompile(`^runner-[0-9a-f]{8}$`)
	// poolVMNamePattern matches poolVMName: slot and creation time.
	poolVMNamePattern = regexp.MustCompile(`^pool-[0-9]+-[0-9]+$`)
	// keepVMNamePattern matches KeepVMName: the throwaway clone that marks a
	// runner image as recently used before a cache prune.
	keepVMNamePattern = regexp.MustCompile(`^runner-keep-[0-9a-f]{8}$`)
)

// NewRunnerName names one ephemeral runner container or VM.
func NewRunnerName() string {
	return "runner-" + uuid.NewString()[:8]
}

// poolVMName names a warm-pool VM by its slot and creation time.
func poolVMName(slot int, created time.Time) string {
	return fmt.Sprintf("pool-%d-%d", slot, created.UnixMilli())
}

// KeepVMName names the throwaway clone a cache prune makes to mark a runner
// image as recently used (see cachestore's Tart store).
func KeepVMName() string {
	return fmt.Sprintf("runner-keep-%08x", rand.Uint32())
}

// OwnsContainer reports whether a listed container is runner's: labelled by
// the current release, or named by one whose label value differed. Docker
// lists names with a leading slash.
func OwnsContainer(labels map[string]string, names []string) bool {
	if labels[ManagedByLabel] == ManagedByValue {
		return true
	}
	for _, name := range names {
		if runnerNamePattern.MatchString(strings.TrimPrefix(name, "/")) {
			return true
		}
	}
	return false
}

// OwnsTartVM reports whether a listed Tart VM is runner's: a local VM named
// as an ephemeral runner, a warm-pool VM or a cache-prune marker clone. OCI
// cache entries are never owned, whatever they are called.
func OwnsTartVM(vm TartVM) bool {
	if vm.Source != "local" {
		return false
	}
	return runnerNamePattern.MatchString(vm.Name) || poolVMNamePattern.MatchString(vm.Name) || keepVMNamePattern.MatchString(vm.Name)
}
