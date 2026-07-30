// +build linux

package specconv

import (
	"testing"

	"github.com/opencontainers/runc/libcontainer/configs/validate"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func TestSpecconvExampleValidate(t *testing.T) {
	spec := Example()
	spec.Root.Path = "/"

	opts := &CreateOpts{
		Spec: spec,
	}

	config, err := CreateLibcontainerConfig(opts)
	if err != nil {
		t.Errorf("Couldn't create libcontainer config: %v", err)
	}

	validator := validate.New()
	if err := validator.Validate(config); err != nil {
		t.Errorf("Expected specconv to produce valid container config: %v", err)
	}
}

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

func TestRootlessSpecconvValidate(t *testing.T) {
	spec := Example()
	spec.Root.Path = "/"
	ToRootless(spec)

	opts := &CreateOpts{
		Spec:     spec,
		Rootless: true,
	}

	config, err := CreateLibcontainerConfig(opts)
	if err != nil {
		t.Errorf("Couldn't create libcontainer config: %v", err)
	}

	validator := validate.New()
	if err := validator.Validate(config); err != nil {
		t.Errorf("Expected specconv to produce valid rootless container config: %v", err)
	}
}
