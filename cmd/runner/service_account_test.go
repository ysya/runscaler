package main

import (
	"context"
	"errors"
	"os"
	osuser "os/user"
	"reflect"
	"slices"
	"testing"
)

func TestEnsureServiceAccountCreatesOnlyTheDedicatedIdentity(t *testing.T) {
	savedRead, savedLookup, savedRun := readServiceIdentityFile, lookupServiceGroup, serviceAdminCommand
	t.Cleanup(func() {
		readServiceIdentityFile = savedRead
		lookupServiceGroup = savedLookup
		serviceAdminCommand = savedRun
	})
	for _, tc := range []struct {
		name                            string
		existing, conflict, missingTool bool
	}{
		{"fresh", false, false, false}, {"compatible reuse", true, false, false}, {"personal account", true, true, false}, {"missing utility", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accountExists, groupExists := tc.existing, tc.existing
			readServiceIdentityFile = func(path string) ([]byte, error) {
				switch path {
				case "/etc/passwd":
					if !accountExists {
						return nil, nil
					}
					if tc.conflict {
						return []byte("runscaler:x:123:456:personal:/home/personal:/bin/sh\n"), nil
					}
					return []byte("runscaler:x:123:456:runner:/var/lib/runner:/usr/sbin/nologin\n"), nil
				case "/etc/group":
					return []byte("runscaler:x:456:\n"), nil
				default:
					return nil, os.ErrNotExist
				}
			}
			lookupServiceGroup = func(name string) (*osuser.Group, error) {
				if name != serviceAccountName {
					t.Fatalf("looked up unrelated group %s", name)
				}
				if !groupExists {
					return nil, osuser.UnknownGroupError(name)
				}
				return &osuser.Group{Name: name, Gid: "456"}, nil
			}
			var commands []string
			serviceAdminCommand = func(_ context.Context, name string, args ...string) error {
				commands = append(commands, name)
				if tc.missingTool {
					return errors.New("tool unavailable")
				}
				switch name {
				case "groupadd":
					groupExists = true
				case "useradd":
					if !slices.Equal(args[:7], []string{"--system", "--gid", "runscaler", "--home-dir", "/var/lib/runner", "--no-create-home", "--shell"}) || args[len(args)-1] != "runscaler" {
						t.Fatalf("unexpected account command %q", args)
					}
					accountExists = true
				default:
					t.Fatalf("unexpected mutation %s %q", name, args)
				}
				return nil
			}
			account, err := ensureServiceAccount(context.Background())
			if tc.conflict || tc.missingTool {
				if err == nil {
					t.Fatal("incompatible preparation succeeded")
				}
				if tc.conflict && len(commands) != 0 {
					t.Fatalf("existing account mutated: %q", commands)
				}
				if tc.missingTool && !reflect.DeepEqual(commands, []string{"groupadd"}) {
					t.Fatalf("continued after missing utility: %q", commands)
				}
				return
			}
			if err != nil || account.UID != 123 || account.GID != 456 {
				t.Fatalf("account=%+v, err=%v", account, err)
			}
			if tc.existing && len(commands) != 0 {
				t.Fatalf("compatible account reconfigured: %q", commands)
			}
			if !tc.existing && !reflect.DeepEqual(commands, []string{"groupadd", "useradd"}) {
				t.Fatalf("creation order %q", commands)
			}
		})
	}
}
