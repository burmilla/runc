//go:build linux
// +build linux

package libcontainer

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/system"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/syndtr/gocapability/capability"
	"github.com/vishvananda/netlink/nl"
)

const stdioFdCount = 3

type linuxContainer struct {
	id                   string
	root                 string
	config               *configs.Config
	initArgs             []string
	initProcess          parentProcess
	initProcessStartTime string
	m                    sync.Mutex
	state                containerState
	created              time.Time
}

// State represents a running container's state
type State struct {
	BaseState

	// Platform specific fields below here

	// Specifies if the container was started under the rootless mode.
	Rootless bool `json:"rootless"`

	// NamespacePaths are filepaths to the container's namespaces. Key is the namespace type
	// with the value as the path.
	NamespacePaths map[configs.NamespaceType]string `json:"namespace_paths"`

	// Container's standard descriptors (std{in,out,err}), needed for checkpoint and restore
	ExternalDescriptors []string `json:"external_descriptors,omitempty"`
}

// Container is a libcontainer container object.
//
// Each container is thread-safe within the same process. Since a container can
// be destroyed by a separate process, any function may return that the container
// was not found.
type Container interface {
	BaseContainer

	// Methods below here are platform specific

	// If the Container state is RUNNING or CREATED, sets the Container state to PAUSING and pauses
	// the execution of any user processes. Asynchronously, when the container finished being paused the
	// state is changed to PAUSED.
	// If the Container state is PAUSED, do nothing.
	//
	// errors:
	// ContainerNotExists - Container no longer exists,
	// ContainerNotRunning - Container not running or created,
	// Systemerror - System error.
	Pause() error

	// If the Container state is PAUSED, resumes the execution of any user processes in the
	// Container before setting the Container state to RUNNING.
	// If the Container state is RUNNING, do nothing.
	//
	// errors:
	// ContainerNotExists - Container no longer exists,
	// ContainerNotPaused - Container is not paused,
	// Systemerror - System error.
	Resume() error
}

// ID returns the container's unique ID
func (c *linuxContainer) ID() string {
	return c.id
}

// Config returns the container's configuration
func (c *linuxContainer) Config() configs.Config {
	return *c.config
}

func (c *linuxContainer) Status() (Status, error) {
	c.m.Lock()
	defer c.m.Unlock()
	return c.currentStatus()
}

func (c *linuxContainer) State() (*State, error) {
	c.m.Lock()
	defer c.m.Unlock()
	return c.currentState()
}

func (c *linuxContainer) Processes() ([]int, error) {
	return []int{}, nil
}

func (c *linuxContainer) Stats() (*Stats, error) {
	var (
		stats = &Stats{}
	)
	for _, iface := range c.config.Networks {
		switch iface.Type {
		case "veth":
			istats, err := getNetworkInterfaceStats(iface.HostInterfaceName)
			if err != nil {
				return stats, newSystemErrorWithCausef(err, "getting network stats for interface %q", iface.HostInterfaceName)
			}
			stats.Interfaces = append(stats.Interfaces, istats)
		}
	}
	return stats, nil
}

func (c *linuxContainer) Set(config configs.Config) error {
	return nil
}

func (c *linuxContainer) Start(process *Process) error {
	c.m.Lock()
	defer c.m.Unlock()
	status, err := c.currentStatus()
	if err != nil {
		return err
	}
	if status == Stopped {
		if err := c.createExecFifo(); err != nil {
			return err
		}
	}
	if err := c.start(process, status == Stopped); err != nil {
		if status == Stopped {
			c.deleteExecFifo()
		}
		return err
	}
	return nil
}

func (c *linuxContainer) Run(process *Process) error {
	c.m.Lock()
	status, err := c.currentStatus()
	if err != nil {
		c.m.Unlock()
		return err
	}
	c.m.Unlock()
	if err := c.Start(process); err != nil {
		return err
	}
	if status == Stopped {
		return c.exec()
	}
	return nil
}

func (c *linuxContainer) Exec() error {
	c.m.Lock()
	defer c.m.Unlock()
	return c.exec()
}

func (c *linuxContainer) exec() error {
	path := filepath.Join(c.root, execFifoFilename)
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return newSystemErrorWithCause(err, "open exec fifo for reading")
	}
	defer f.Close()
	data, err := ioutil.ReadAll(f)
	if err != nil {
		return err
	}
	if len(data) > 0 {
		os.Remove(path)
		return nil
	}
	return fmt.Errorf("cannot start an already running container")
}

func (c *linuxContainer) start(process *Process, isInit bool) error {
	parent, err := c.newParentProcess(process, isInit)
	if err != nil {
		return newSystemErrorWithCause(err, "creating new parent process")
	}
	if err := parent.start(); err != nil {
		// terminate the process to ensure that it properly is reaped.
		if err := parent.terminate(); err != nil {
			logrus.Warn(err)
		}
		return newSystemErrorWithCause(err, "starting container process")
	}
	// generate a timestamp indicating when the container was started
	c.created = time.Now().UTC()
	if isInit {
		c.state = &createdState{
			c: c,
		}
		state, err := c.updateState(parent)
		if err != nil {
			return err
		}
		c.initProcessStartTime = state.InitProcessStartTime

		if c.config.Hooks != nil {
			s := configs.HookState{
				Version: c.config.Version,
				ID:      c.id,
				Pid:     parent.pid(),
				Bundle:  utils.SearchLabels(c.config.Labels, "bundle"),
			}
			for i, hook := range c.config.Hooks.Poststart {
				if err := hook.Run(s); err != nil {
					if err := parent.terminate(); err != nil {
						logrus.Warn(err)
					}
					return newSystemErrorWithCausef(err, "running poststart hook %d", i)
				}
			}
		}
	} else {
		c.state = &runningState{
			c: c,
		}
	}
	return nil
}

func (c *linuxContainer) Signal(s os.Signal, all bool) error {
	if err := c.initProcess.signal(s); err != nil {
		return newSystemErrorWithCause(err, "signaling init process")
	}
	return nil
}

func (c *linuxContainer) createExecFifo() error {
	rootuid, err := c.Config().HostRootUID()
	if err != nil {
		return err
	}
	rootgid, err := c.Config().HostRootGID()
	if err != nil {
		return err
	}

	fifoName := filepath.Join(c.root, execFifoFilename)
	if _, err := os.Stat(fifoName); err == nil {
		return fmt.Errorf("exec fifo %s already exists", fifoName)
	}
	oldMask := syscall.Umask(0000)
	if err := syscall.Mkfifo(fifoName, 0622); err != nil {
		syscall.Umask(oldMask)
		return err
	}
	syscall.Umask(oldMask)
	if err := os.Chown(fifoName, rootuid, rootgid); err != nil {
		return err
	}
	return nil
}

func (c *linuxContainer) deleteExecFifo() {
	fifoName := filepath.Join(c.root, execFifoFilename)
	os.Remove(fifoName)
}

func (c *linuxContainer) newParentProcess(p *Process, doInit bool) (parentProcess, error) {
	parentPipe, childPipe, err := utils.NewSockPair("init")
	if err != nil {
		return nil, newSystemErrorWithCause(err, "creating new init pipe")
	}
	cmd, err := c.commandTemplate(p, childPipe)
	if err != nil {
		return nil, newSystemErrorWithCause(err, "creating new command template")
	}
	if !doInit {
		return c.newSetnsProcess(p, cmd, parentPipe, childPipe)
	}

	// We only set up rootDir if we're not doing a `runc exec`. The reason for
	// this is to avoid cases where a racing, unprivileged process inside the
	// container can get access to the statedir file descriptor (which would
	// allow for container rootfs escape).
	rootDir, err := os.Open(c.root)
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, rootDir)
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("_LIBCONTAINER_STATEDIR=%d", stdioFdCount+len(cmd.ExtraFiles)-1))
	return c.newInitProcess(p, cmd, parentPipe, childPipe, rootDir)
}

func (c *linuxContainer) commandTemplate(p *Process, childPipe *os.File) (*exec.Cmd, error) {
	cmd := exec.Command(c.initArgs[0], c.initArgs[1:]...)
	cmd.Stdin = p.Stdin
	cmd.Stdout = p.Stdout
	cmd.Stderr = p.Stderr
	cmd.Dir = c.config.Rootfs
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, p.ExtraFiles...)
	if p.ConsoleSocket != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, p.ConsoleSocket)
		cmd.Env = append(cmd.Env,
			fmt.Sprintf("_LIBCONTAINER_CONSOLE=%d", stdioFdCount+len(cmd.ExtraFiles)-1),
		)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, childPipe)
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("_LIBCONTAINER_INITPIPE=%d", stdioFdCount+len(cmd.ExtraFiles)-1),
	)
	// NOTE: when running a container with no PID namespace and the parent process spawning the container is
	// PID1 the pdeathsig is being delivered to the container's init process by the kernel for some reason
	// even with the parent still running.
	if c.config.ParentDeathSignal > 0 {
		cmd.SysProcAttr.Pdeathsig = syscall.Signal(c.config.ParentDeathSignal)
	}
	return cmd, nil
}

func (c *linuxContainer) newInitProcess(p *Process, cmd *exec.Cmd, parentPipe, childPipe, rootDir *os.File) (*initProcess, error) {
	cmd.Env = append(cmd.Env, "_LIBCONTAINER_INITTYPE="+string(initStandard))
	nsMaps := make(map[configs.NamespaceType]string)
	for _, ns := range c.config.Namespaces {
		if ns.Path != "" {
			nsMaps[ns.Type] = ns.Path
		}
	}
	_, sharePidns := nsMaps[configs.NEWPID]
	data, err := c.bootstrapData(c.config.Namespaces.CloneFlags(), nsMaps)
	if err != nil {
		return nil, err
	}
	return &initProcess{
		cmd:           cmd,
		childPipe:     childPipe,
		parentPipe:    parentPipe,
		config:        c.newInitConfig(p),
		container:     c,
		process:       p,
		bootstrapData: data,
		sharePidns:    sharePidns,
		rootDir:       rootDir,
	}, nil
}

func (c *linuxContainer) newSetnsProcess(p *Process, cmd *exec.Cmd, parentPipe, childPipe *os.File) (*setnsProcess, error) {
	cmd.Env = append(cmd.Env, "_LIBCONTAINER_INITTYPE="+string(initSetns))
	state, err := c.currentState()
	if err != nil {
		return nil, newSystemErrorWithCause(err, "getting container's current state")
	}
	// for setns process, we don't have to set cloneflags as the process namespaces
	// will only be set via setns syscall
	data, err := c.bootstrapData(0, state.NamespacePaths)
	if err != nil {
		return nil, err
	}
	return &setnsProcess{
		cmd:           cmd,
		childPipe:     childPipe,
		parentPipe:    parentPipe,
		config:        c.newInitConfig(p),
		process:       p,
		bootstrapData: data,
	}, nil
}

func (c *linuxContainer) newInitConfig(process *Process) *initConfig {
	cfg := &initConfig{
		Config:           c.config,
		Args:             process.Args,
		Env:              process.Env,
		User:             process.User,
		AdditionalGroups: process.AdditionalGroups,
		Cwd:              process.Cwd,
		Capabilities:     process.Capabilities,
		PassedFilesCount: len(process.ExtraFiles),
		ContainerId:      c.ID(),
		NoNewPrivileges:  c.config.NoNewPrivileges,
		Rootless:         c.config.Rootless,
		AppArmorProfile:  c.config.AppArmorProfile,
		ProcessLabel:     c.config.ProcessLabel,
		Rlimits:          c.config.Rlimits,
	}
	if process.NoNewPrivileges != nil {
		cfg.NoNewPrivileges = *process.NoNewPrivileges
	}
	if process.AppArmorProfile != "" {
		cfg.AppArmorProfile = process.AppArmorProfile
	}
	if process.Label != "" {
		cfg.ProcessLabel = process.Label
	}
	if len(process.Rlimits) > 0 {
		cfg.Rlimits = process.Rlimits
	}
	cfg.CreateConsole = process.ConsoleSocket != nil
	return cfg
}

func (c *linuxContainer) Destroy() error {
	c.m.Lock()
	defer c.m.Unlock()
	return c.state.destroy()
}

func (c *linuxContainer) Pause() error {
	c.m.Lock()
	defer c.m.Unlock()
	status, err := c.currentStatus()
	if err != nil {
		return err
	}
	switch status {
	case Running, Created:
		return c.state.transition(&pausedState{
			c: c,
		})
	}
	return newGenericError(fmt.Errorf("container not running or created: %s", status), ContainerNotRunning)
}

func (c *linuxContainer) Resume() error {
	c.m.Lock()
	defer c.m.Unlock()
	status, err := c.currentStatus()
	if err != nil {
		return err
	}
	if status != Paused {
		return newGenericError(fmt.Errorf("container not paused"), ContainerNotPaused)
	}
	return c.state.transition(&runningState{
		c: c,
	})
}

func (c *linuxContainer) updateState(process parentProcess) (*State, error) {
	c.initProcess = process
	state, err := c.currentState()
	if err != nil {
		return nil, err
	}
	err = c.saveState(state)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (c *linuxContainer) saveState(s *State) error {
	f, err := os.Create(filepath.Join(c.root, stateFilename))
	if err != nil {
		return err
	}
	defer f.Close()
	return utils.WriteJSON(f, s)
}

func (c *linuxContainer) deleteState() error {
	return os.Remove(filepath.Join(c.root, stateFilename))
}

func (c *linuxContainer) currentStatus() (Status, error) {
	if err := c.refreshState(); err != nil {
		return -1, err
	}
	return c.state.status(), nil
}

// refreshState needs to be called to verify that the current state on the
// container is what is true.  Because consumers of libcontainer can use it
// out of process we need to verify the container's status based on runtime
// information and not rely on our in process info.
func (c *linuxContainer) refreshState() error {
	paused, err := c.isPaused()
	if err != nil {
		return err
	}
	if paused {
		return c.state.transition(&pausedState{c: c})
	}
	t, err := c.runType()
	if err != nil {
		return err
	}
	switch t {
	case Created:
		return c.state.transition(&createdState{c: c})
	case Running:
		return c.state.transition(&runningState{c: c})
	}
	return c.state.transition(&stoppedState{c: c})
}

// doesInitProcessExist checks if the init process is still the same process
// as the initial one, it could happen that the original process has exited
// and a new process has been created with the same pid, in this case, the
// container would already be stopped.
func (c *linuxContainer) doesInitProcessExist(initPid int) (bool, error) {
	startTime, err := system.GetProcessStartTime(initPid)
	if err != nil {
		return false, newSystemErrorWithCausef(err, "getting init process %d start time", initPid)
	}
	if c.initProcessStartTime != startTime {
		return false, nil
	}
	return true, nil
}

func (c *linuxContainer) runType() (Status, error) {
	if c.initProcess == nil {
		return Stopped, nil
	}
	pid := c.initProcess.pid()
	// return Running if the init process is alive
	if err := syscall.Kill(pid, 0); err != nil {
		if err == syscall.ESRCH {
			// It means the process does not exist anymore, could happen when the
			// process exited just when we call the function, we should not return
			// error in this case.
			return Stopped, nil
		}
		return Stopped, newSystemErrorWithCausef(err, "sending signal 0 to pid %d", pid)
	}
	// check if the process is still the original init process.
	exist, err := c.doesInitProcessExist(pid)
	if !exist || err != nil {
		return Stopped, err
	}
	// We'll create exec fifo and blocking on it after container is created,
	// and delete it after start container.
	if _, err := os.Stat(filepath.Join(c.root, execFifoFilename)); err == nil {
		return Created, nil
	}
	return Running, nil
}

func (c *linuxContainer) isPaused() (bool, error) {
	return false, nil
}

func (c *linuxContainer) currentState() (*State, error) {
	var (
		startTime           string
		externalDescriptors []string
		pid                 = -1
	)
	if c.initProcess != nil {
		pid = c.initProcess.pid()
		startTime, _ = c.initProcess.startTime()
		externalDescriptors = c.initProcess.externalDescriptors()
	}
	state := &State{
		BaseState: BaseState{
			ID:                   c.ID(),
			Config:               *c.config,
			InitProcessPid:       pid,
			InitProcessStartTime: startTime,
			Created:              c.created,
		},
		Rootless:            c.config.Rootless,
		NamespacePaths:      make(map[configs.NamespaceType]string),
		ExternalDescriptors: externalDescriptors,
	}
	if pid > 0 {
		for _, ns := range c.config.Namespaces {
			state.NamespacePaths[ns.Type] = ns.GetPath(pid)
		}
		for _, nsType := range configs.NamespaceTypes() {
			if !configs.IsNamespaceSupported(nsType) {
				continue
			}
			if _, ok := state.NamespacePaths[nsType]; !ok {
				ns := configs.Namespace{Type: nsType}
				state.NamespacePaths[ns.Type] = ns.GetPath(pid)
			}
		}
	}
	return state, nil
}

// orderNamespacePaths sorts namespace paths into a list of paths that we
// can setns in order.
func (c *linuxContainer) orderNamespacePaths(namespaces map[configs.NamespaceType]string) ([]string, error) {
	paths := []string{}

	for _, ns := range configs.NamespaceTypes() {

		// Remove namespaces that we don't need to join.
		if !c.config.Namespaces.Contains(ns) {
			continue
		}

		if p, ok := namespaces[ns]; ok && p != "" {
			// check if the requested namespace is supported
			if !configs.IsNamespaceSupported(ns) {
				return nil, newSystemError(fmt.Errorf("namespace %s is not supported", ns))
			}
			// only set to join this namespace if it exists
			if _, err := os.Lstat(p); err != nil {
				return nil, newSystemErrorWithCausef(err, "running lstat on namespace path %q", p)
			}
			// do not allow namespace path with comma as we use it to separate
			// the namespace paths
			if strings.ContainsRune(p, ',') {
				return nil, newSystemError(fmt.Errorf("invalid path %s", p))
			}
			paths = append(paths, fmt.Sprintf("%s:%s", configs.NsName(ns), p))
		}

	}

	return paths, nil
}

func encodeIDMapping(idMap []configs.IDMap) ([]byte, error) {
	data := bytes.NewBuffer(nil)
	for _, im := range idMap {
		line := fmt.Sprintf("%d %d %d\n", im.ContainerID, im.HostID, im.Size)
		if _, err := data.WriteString(line); err != nil {
			return nil, err
		}
	}
	return data.Bytes(), nil
}

// bootstrapData encodes the necessary data in netlink binary format
// as a io.Reader.
// Consumer can write the data to a bootstrap program
// such as one that uses nsenter package to bootstrap the container's
// init process correctly, i.e. with correct namespaces, uid/gid
// mapping etc.
func (c *linuxContainer) bootstrapData(cloneFlags uintptr, nsMaps map[configs.NamespaceType]string) (io.Reader, error) {
	// create the netlink message
	r := nl.NewNetlinkRequest(int(InitMsg), 0)

	// write cloneFlags
	r.AddData(&Int32msg{
		Type:  CloneFlagsAttr,
		Value: uint32(cloneFlags),
	})

	// write custom namespace paths
	if len(nsMaps) > 0 {
		nsPaths, err := c.orderNamespacePaths(nsMaps)
		if err != nil {
			return nil, err
		}
		r.AddData(&Bytemsg{
			Type:  NsPathsAttr,
			Value: []byte(strings.Join(nsPaths, ",")),
		})
	}

	// write namespace paths only when we are not joining an existing user ns
	_, joinExistingUser := nsMaps[configs.NEWUSER]
	if !joinExistingUser {
		// write uid mappings
		if len(c.config.UidMappings) > 0 {
			b, err := encodeIDMapping(c.config.UidMappings)
			if err != nil {
				return nil, err
			}
			r.AddData(&Bytemsg{
				Type:  UidmapAttr,
				Value: b,
			})
		}

		// write gid mappings
		if len(c.config.GidMappings) > 0 {
			b, err := encodeIDMapping(c.config.GidMappings)
			if err != nil {
				return nil, err
			}
			r.AddData(&Bytemsg{
				Type:  GidmapAttr,
				Value: b,
			})
			// The following only applies if we are root.
			if !c.config.Rootless {
				// check if we have CAP_SETGID to setgroup properly
				pid, err := capability.NewPid(os.Getpid())
				if err != nil {
					return nil, err
				}
				if !pid.Get(capability.EFFECTIVE, capability.CAP_SETGID) {
					r.AddData(&Boolmsg{
						Type:  SetgroupAttr,
						Value: true,
					})
				}
			}
		}
	}

	// write oom_score_adj
	r.AddData(&Bytemsg{
		Type:  OomScoreAdjAttr,
		Value: []byte(fmt.Sprintf("%d", c.config.OomScoreAdj)),
	})

	// write rootless
	r.AddData(&Boolmsg{
		Type:  RootlessAttr,
		Value: c.config.Rootless,
	})

	return bytes.NewReader(r.Serialize()), nil
}
