// +build linux

package specconv

import (
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
)

func TestDupNamespaces(t *testing.T) {
	spec := &specs.Spec{
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{
					Type: "pid",
				},
				{
					Type: "pid",
					Path: "/proc/1/ns/pid",
				},
			},
		},
	}

	_, err := CreateLibcontainerConfig(&CreateOpts{
		Spec: spec,
	})

	if err == nil {
		t.Errorf("Duplicated namespaces should be forbidden")
	}
}

