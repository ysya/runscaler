package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestReadSystemdTargetLoadsInactiveUnitBeforeReadingEffectiveProperties(t *testing.T) {
	saved := installationOutput
	t.Cleanup(func() { installationOutput = saved })
	loaded := false
	installationOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "busctl" {
			t.Fatalf("unexpected program %s", name)
		}
		if slices.Contains(args, "LoadUnit") {
			loaded = true
			return []byte(`{"type":"o","data":["/org/freedesktop/systemd1/unit/runner_2eservice"]}`), nil
		}
		if !loaded {
			return nil, errors.New("unit is not loaded")
		}
		property := args[len(args)-1]
		if property == "ExecStartEx" {
			return []byte(`{"type":"a(sasasttttuii)","data":[["/usr/local/bin/runner",["/usr/local/bin/runner","run","--config","/etc/runner/config.toml"],[],0,0,0,0,0,0,0]]}`), nil
		}
		if property == "ReadWritePaths" {
			return []byte(`{"type":"as","data":["/tmp"]}`), nil
		}
		values := map[string]string{
			"ExecStart": `{"type":"a(sasbttttuii)","data":[["/usr/local/bin/runner",["/usr/local/bin/runner","run","--config","/etc/runner/config.toml"],false,0,0,0,0,0,0,0]]}`,
			"User":      `{"type":"s","data":"runscaler"}`, "Group": `{"type":"s","data":"runscaler"}`, "Environment": `{"type":"as","data":[]}`, "SupplementaryGroups": `{"type":"as","data":["998"]}`, "WorkingDirectory": `{"type":"s","data":"/var/lib/runner"}`, "EnvironmentFiles": `{"type":"a(sb)","data":[]}`,
		}
		data := []byte(values[property])
		if !json.Valid(data) {
			t.Fatalf("unexpected property %s", property)
		}
		return data, nil
	}
	got, err := readSystemdTarget(context.Background(), scopeSystem)
	if err != nil || got.RuntimeUser != "runscaler" || !reflect.DeepEqual(got.SupplementaryGroups, []string{"998"}) {
		t.Fatalf("inactive target=%+v, %v", got, err)
	}
}
