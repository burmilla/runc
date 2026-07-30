// +build linux

// Package specconv implements conversion of specifications to libcontainer
// configurations
package specconv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/seccomp"
	"github.com/opencontainers/runtime-spec/specs-go"
)

var namespaceMapping = map[specs.LinuxNamespaceType]configs.NamespaceType{
	specs.PIDNamespace:     configs.NEWPID,
	specs.NetworkNamespace: configs.NEWNET,
	specs.MountNamespace:   configs.NEWNS,
	specs.UserNamespace:    configs.NEWUSER,
	specs.IPCNamespace:     configs.NEWIPC,
	specs.UTSNamespace:     configs.NEWUTS,
}

var mountPropagationMapping = map[string]int{
	"rprivate": syscall.MS_PRIVATE | syscall.MS_REC,
	"private":  syscall.MS_PRIVATE,
	"rslave":   syscall.MS_SLAVE | syscall.MS_REC,
	"slave":    syscall.MS_SLAVE,
	"rshared":  syscall.MS_SHARED | syscall.MS_REC,
	"shared":   syscall.MS_SHARED,
	"":         syscall.MS_PRIVATE | syscall.MS_REC,
}

type CreateOpts struct {
	NoPivotRoot  bool
	NoNewKeyring bool
	Spec         *specs.Spec
	Rootless     bool
}

// CreateLibcontainerConfig creates a new libcontainer configuration from a
// given specification and a cgroup name
func CreateLibcontainerConfig(opts *CreateOpts) (*configs.Config, error) {
	// runc's cwd will always be the bundle path
	rcwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cwd, err := filepath.Abs(rcwd)
	if err != nil {
		return nil, err
	}
	spec := opts.Spec
	rootfsPath := spec.Root.Path
	if !filepath.IsAbs(rootfsPath) {
		rootfsPath = filepath.Join(cwd, rootfsPath)
	}
	labels := []string{}
	for k, v := range spec.Annotations {
		labels = append(labels, fmt.Sprintf("%s=%s", k, v))
	}
	config := &configs.Config{
		Rootfs:       rootfsPath,
		NoPivotRoot:  opts.NoPivotRoot,
		Readonlyfs:   spec.Root.Readonly,
		Hostname:     spec.Hostname,
		Labels:       append(labels, fmt.Sprintf("bundle=%s", cwd)),
		NoNewKeyring: opts.NoNewKeyring,
		Rootless:     opts.Rootless,
	}

	exists := false
	if config.RootPropagation, exists = mountPropagationMapping[spec.Linux.RootfsPropagation]; !exists {
		return nil, fmt.Errorf("rootfsPropagation=%v is not supported", spec.Linux.RootfsPropagation)
	}

	for _, ns := range spec.Linux.Namespaces {
		t, exists := namespaceMapping[ns.Type]
		if !exists {
			return nil, fmt.Errorf("namespace %q does not exist", ns)
		}
		if config.Namespaces.Contains(t) {
			return nil, fmt.Errorf("malformed spec file: duplicated ns %q", ns)
		}
		config.Namespaces.Add(t, ns.Path)
	}
	if config.Namespaces.Contains(configs.NEWNET) {
		config.Networks = []*configs.Network{
			{
				Type: "loopback",
			},
		}
	}
	for _, m := range spec.Mounts {
		config.Mounts = append(config.Mounts, createLibcontainerMount(cwd, m))
	}
	if err := createDevices(spec, config); err != nil {
		return nil, err
	}
	if err := setupUserNamespace(spec, config); err != nil {
		return nil, err
	}
	// set extra path masking for libcontainer for the various unsafe places in proc
	config.MaskPaths = spec.Linux.MaskedPaths
	config.ReadonlyPaths = spec.Linux.ReadonlyPaths
	if spec.Linux.Seccomp != nil {
		seccomp, err := setupSeccomp(spec.Linux.Seccomp)
		if err != nil {
			return nil, err
		}
		config.Seccomp = seccomp
	}
	if spec.Process.SelinuxLabel != "" {
		config.ProcessLabel = spec.Process.SelinuxLabel
	}
	config.Sysctl = spec.Linux.Sysctl
	if spec.Linux.Resources != nil && spec.Linux.Resources.OOMScoreAdj != nil {
		config.OomScoreAdj = *spec.Linux.Resources.OOMScoreAdj
	}
	if spec.Process.Capabilities != nil {
		config.Capabilities = &configs.Capabilities{
			Bounding:    spec.Process.Capabilities.Bounding,
			Effective:   spec.Process.Capabilities.Effective,
			Permitted:   spec.Process.Capabilities.Permitted,
			Inheritable: spec.Process.Capabilities.Inheritable,
			Ambient:     spec.Process.Capabilities.Ambient,
		}
	}
	createHooks(spec, config)
	config.MountLabel = spec.Linux.MountLabel
	config.Version = specs.Version
	return config, nil
}

func createLibcontainerMount(cwd string, m specs.Mount) *configs.Mount {
	flags, pgflags, data, ext := parseMountOptions(m.Options)
	source := m.Source
	if m.Type == "bind" {
		if !filepath.IsAbs(source) {
			source = filepath.Join(cwd, m.Source)
		}
	}
	return &configs.Mount{
		Device:           m.Type,
		Source:           source,
		Destination:      m.Destination,
		Data:             data,
		Flags:            flags,
		PropagationFlags: pgflags,
		Extensions:       ext,
	}
}

func stringToDeviceRune(s string) (rune, error) {
	switch s {
	case "p":
		return 'p', nil
	case "u":
		return 'u', nil
	case "b":
		return 'b', nil
	case "c":
		return 'c', nil
	default:
		return 0, fmt.Errorf("invalid device type %q", s)
	}
}

func createDevices(spec *specs.Spec, config *configs.Config) error {
	// add whitelisted devices
	config.Devices = []*configs.Device{
		{
			Type:     'c',
			Path:     "/dev/null",
			Major:    1,
			Minor:    3,
			FileMode: 0666,
			Uid:      0,
			Gid:      0,
		},
		{
			Type:     'c',
			Path:     "/dev/random",
			Major:    1,
			Minor:    8,
			FileMode: 0666,
			Uid:      0,
			Gid:      0,
		},
		{
			Type:     'c',
			Path:     "/dev/full",
			Major:    1,
			Minor:    7,
			FileMode: 0666,
			Uid:      0,
			Gid:      0,
		},
		{
			Type:     'c',
			Path:     "/dev/tty",
			Major:    5,
			Minor:    0,
			FileMode: 0666,
			Uid:      0,
			Gid:      0,
		},
		{
			Type:     'c',
			Path:     "/dev/zero",
			Major:    1,
			Minor:    5,
			FileMode: 0666,
			Uid:      0,
			Gid:      0,
		},
		{
			Type:     'c',
			Path:     "/dev/urandom",
			Major:    1,
			Minor:    9,
			FileMode: 0666,
			Uid:      0,
			Gid:      0,
		},
	}
	// merge in additional devices from the spec
	for _, d := range spec.Linux.Devices {
		var uid, gid uint32
		var filemode os.FileMode = 0666

		if d.UID != nil {
			uid = *d.UID
		}
		if d.GID != nil {
			gid = *d.GID
		}
		dt, err := stringToDeviceRune(d.Type)
		if err != nil {
			return err
		}
		if d.FileMode != nil {
			filemode = *d.FileMode
		}
		device := &configs.Device{
			Type:     dt,
			Path:     d.Path,
			Major:    d.Major,
			Minor:    d.Minor,
			FileMode: filemode,
			Uid:      uid,
			Gid:      gid,
		}
		config.Devices = append(config.Devices, device)
	}
	return nil
}

func setupUserNamespace(spec *specs.Spec, config *configs.Config) error {
	if len(spec.Linux.UIDMappings) == 0 {
		return nil
	}
	create := func(m specs.LinuxIDMapping) configs.IDMap {
		return configs.IDMap{
			HostID:      int(m.HostID),
			ContainerID: int(m.ContainerID),
			Size:        int(m.Size),
		}
	}
	for _, m := range spec.Linux.UIDMappings {
		config.UidMappings = append(config.UidMappings, create(m))
	}
	for _, m := range spec.Linux.GIDMappings {
		config.GidMappings = append(config.GidMappings, create(m))
	}
	rootUID, err := config.HostRootUID()
	if err != nil {
		return err
	}
	rootGID, err := config.HostRootGID()
	if err != nil {
		return err
	}
	for _, node := range config.Devices {
		node.Uid = uint32(rootUID)
		node.Gid = uint32(rootGID)
	}
	return nil
}

// parseMountOptions parses the string and returns the flags, propagation
// flags and any mount data that it contains.
func parseMountOptions(options []string) (int, []int, string, int) {
	var (
		flag     int
		pgflag   []int
		data     []string
		extFlags int
	)
	flags := map[string]struct {
		clear bool
		flag  int
	}{
		"async":         {true, syscall.MS_SYNCHRONOUS},
		"atime":         {true, syscall.MS_NOATIME},
		"bind":          {false, syscall.MS_BIND},
		"defaults":      {false, 0},
		"dev":           {true, syscall.MS_NODEV},
		"diratime":      {true, syscall.MS_NODIRATIME},
		"dirsync":       {false, syscall.MS_DIRSYNC},
		"exec":          {true, syscall.MS_NOEXEC},
		"mand":          {false, syscall.MS_MANDLOCK},
		"noatime":       {false, syscall.MS_NOATIME},
		"nodev":         {false, syscall.MS_NODEV},
		"nodiratime":    {false, syscall.MS_NODIRATIME},
		"noexec":        {false, syscall.MS_NOEXEC},
		"nomand":        {true, syscall.MS_MANDLOCK},
		"norelatime":    {true, syscall.MS_RELATIME},
		"nostrictatime": {true, syscall.MS_STRICTATIME},
		"nosuid":        {false, syscall.MS_NOSUID},
		"rbind":         {false, syscall.MS_BIND | syscall.MS_REC},
		"relatime":      {false, syscall.MS_RELATIME},
		"remount":       {false, syscall.MS_REMOUNT},
		"ro":            {false, syscall.MS_RDONLY},
		"rw":            {true, syscall.MS_RDONLY},
		"strictatime":   {false, syscall.MS_STRICTATIME},
		"suid":          {true, syscall.MS_NOSUID},
		"sync":          {false, syscall.MS_SYNCHRONOUS},
	}
	propagationFlags := map[string]int{
		"private":     syscall.MS_PRIVATE,
		"shared":      syscall.MS_SHARED,
		"slave":       syscall.MS_SLAVE,
		"unbindable":  syscall.MS_UNBINDABLE,
		"rprivate":    syscall.MS_PRIVATE | syscall.MS_REC,
		"rshared":     syscall.MS_SHARED | syscall.MS_REC,
		"rslave":      syscall.MS_SLAVE | syscall.MS_REC,
		"runbindable": syscall.MS_UNBINDABLE | syscall.MS_REC,
	}
	extensionFlags := map[string]struct {
		clear bool
		flag  int
	}{
		"tmpcopyup": {false, configs.EXT_COPYUP},
	}
	for _, o := range options {
		// If the option does not exist in the flags table or the flag
		// is not supported on the platform,
		// then it is a data value for a specific fs type
		if f, exists := flags[o]; exists && f.flag != 0 {
			if f.clear {
				flag &= ^f.flag
			} else {
				flag |= f.flag
			}
		} else if f, exists := propagationFlags[o]; exists && f != 0 {
			pgflag = append(pgflag, f)
		} else if f, exists := extensionFlags[o]; exists && f.flag != 0 {
			if f.clear {
				extFlags &= ^f.flag
			} else {
				extFlags |= f.flag
			}
		} else {
			data = append(data, o)
		}
	}
	return flag, pgflag, strings.Join(data, ","), extFlags
}

func setupSeccomp(config *specs.LinuxSeccomp) (*configs.Seccomp, error) {
	if config == nil {
		return nil, nil
	}

	// No default action specified, no syscalls listed, assume seccomp disabled
	if config.DefaultAction == "" && len(config.Syscalls) == 0 {
		return nil, nil
	}

	newConfig := new(configs.Seccomp)
	newConfig.Syscalls = []*configs.Syscall{}

	if len(config.Architectures) > 0 {
		newConfig.Architectures = []string{}
		for _, arch := range config.Architectures {
			newArch, err := seccomp.ConvertStringToArch(string(arch))
			if err != nil {
				return nil, err
			}
			newConfig.Architectures = append(newConfig.Architectures, newArch)
		}
	}

	// Convert default action from string representation
	newDefaultAction, err := seccomp.ConvertStringToAction(string(config.DefaultAction))
	if err != nil {
		return nil, err
	}
	newConfig.DefaultAction = newDefaultAction

	// Loop through all syscall blocks and convert them to libcontainer format
	for _, call := range config.Syscalls {
		newAction, err := seccomp.ConvertStringToAction(string(call.Action))
		if err != nil {
			return nil, err
		}

		for _, name := range call.Names {
			newCall := configs.Syscall{
				Name:   name,
				Action: newAction,
				Args:   []*configs.Arg{},
			}
			// Loop through all the arguments of the syscall and convert them
			for _, arg := range call.Args {
				newOp, err := seccomp.ConvertStringToOperator(string(arg.Op))
				if err != nil {
					return nil, err
				}

				newArg := configs.Arg{
					Index:    arg.Index,
					Value:    arg.Value,
					ValueTwo: arg.ValueTwo,
					Op:       newOp,
				}

				newCall.Args = append(newCall.Args, &newArg)
			}
			newConfig.Syscalls = append(newConfig.Syscalls, &newCall)
		}
	}

	return newConfig, nil
}

func createHooks(rspec *specs.Spec, config *configs.Config) {
	config.Hooks = &configs.Hooks{}
	if rspec.Hooks != nil {

		for _, h := range rspec.Hooks.Prestart {
			cmd := createCommandHook(h)
			config.Hooks.Prestart = append(config.Hooks.Prestart, configs.NewCommandHook(cmd))
		}
		for _, h := range rspec.Hooks.Poststart {
			cmd := createCommandHook(h)
			config.Hooks.Poststart = append(config.Hooks.Poststart, configs.NewCommandHook(cmd))
		}
		for _, h := range rspec.Hooks.Poststop {
			cmd := createCommandHook(h)
			config.Hooks.Poststop = append(config.Hooks.Poststop, configs.NewCommandHook(cmd))
		}
	}
}

func createCommandHook(h specs.Hook) configs.Command {
	cmd := configs.Command{
		Path: h.Path,
		Args: h.Args,
		Env:  h.Env,
	}
	if h.Timeout != nil {
		d := time.Duration(*h.Timeout) * time.Second
		cmd.Timeout = &d
	}
	return cmd
}
