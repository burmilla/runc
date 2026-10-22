// +build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// loadSpec loads the specification from the provided path.
func loadSpec(cPath string) (spec *specs.Spec, err error) {
	cf, err := os.Open(cPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("JSON specification file %s not found", cPath)
		}
		return nil, err
	}
	defer cf.Close()

	if err = json.NewDecoder(cf).Decode(&spec); err != nil {
		return nil, err
	}
	if err = validatePlatform(&spec.Platform); err != nil {
		return nil, err
	}
	return spec, validateProcessSpec(&spec.Process)
}

func createLibContainerRlimit(rlimit specs.LinuxRlimit) (configs.Rlimit, error) {
	rl, err := strToRlimit(rlimit.Type)
	if err != nil {
		return configs.Rlimit{}, err
	}
	return configs.Rlimit{
		Type: rl,
		Hard: rlimit.Hard,
		Soft: rlimit.Soft,
	}, nil
}

func validatePlatform(platform *specs.Platform) error {
	if platform.OS != runtime.GOOS {
		return fmt.Errorf("target os %s mismatch with current os %s", platform.OS, runtime.GOOS)
	}
	if platform.Arch != runtime.GOARCH {
		return fmt.Errorf("target arch %s mismatch with current arch %s", platform.Arch, runtime.GOARCH)
	}
	return nil
}
