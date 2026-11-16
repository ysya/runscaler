package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/moby/moby/api/types/volume"
)

func TestFindOrphanContainersDinDPartialGroups(t *testing.T) {
	labels := func(group, role string) map[string]string {
		return map[string]string{"runscaler.dind-managed": "true", "runscaler.dind-instance": group, "runscaler.dind-role": role}
	}
	runner := labels("with-runner", "runner")
	runner["managed-by"] = "runner"
	m := &orphanFake{containers: []fakeContainer{{id: "runner", labels: runner}, {id: "daemon-a", labels: labels("with-runner", "daemon")}, {id: "daemon-b", labels: labels("daemon-only", "daemon")}}, cleanupVolumes: []volume.Volume{{Name: "data", Labels: labels("volume-only", "data")}, {Name: "unrelated"}}}
	orphans, err := findOrphanContainers(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 3 {
		t.Fatalf("want one entry per group, got %+v", orphans)
	}
	for _, o := range orphans {
		if o.DinDInstance == "" {
			t.Fatalf("missing durable group identity: %+v", o)
		}
	}
	if n := removeOrphanContainers(context.Background(), m, orphans, slog.New(slog.DiscardHandler)); n != 3 {
		t.Fatalf("removed %d groups", n)
	}
	if len(m.removed) != 3 || len(m.volumesRemoved) != 1 || m.volumesRemoved[0] != "data" {
		t.Fatalf("incomplete or unrelated removal: %v %v", m.removed, m.volumesRemoved)
	}
}
