// +build linux

package libcontainer

import (
	"fmt"
	"os"
	"testing"

	"github.com/opencontainers/runc/libcontainer/configs"
)

type mockProcess struct {
	_pid    int
	started string
}

func (m *mockProcess) terminate() error {
	return nil
}

func (m *mockProcess) pid() int {
	return m._pid
}

func (m *mockProcess) startTime() (string, error) {
	return m.started, nil
}

func (m *mockProcess) start() error {
	return nil
}

func (m *mockProcess) wait() (*os.ProcessState, error) {
	return nil, nil
}

func (m *mockProcess) signal(_ os.Signal) error {
	return nil
}

func (m *mockProcess) externalDescriptors() []string {
	return []string{}
}

func (m *mockProcess) setExternalDescriptors(newFds []string) {
}

func TestGetContainerPids(t *testing.T) {
	container := &linuxContainer{
		id:     "myid",
		config: &configs.Config{},
	}
	pids, err := container.Processes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 0 {
		t.Fatalf("expected no pids without cgroups but received %v", pids)
	}
}

func TestGetContainerStats(t *testing.T) {
	container := &linuxContainer{
		id:     "myid",
		config: &configs.Config{},
	}
	stats, err := container.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.CgroupStats != nil {
		t.Fatal("cgroup stats should be nil since this build has no cgroups dependency")
	}
}

func TestGetContainerState(t *testing.T) {
	var (
		pid                 = os.Getpid()
		expectedNetworkPath = "/networks/fd"
	)
	container := &linuxContainer{
		id: "myid",
		config: &configs.Config{
			Namespaces: []configs.Namespace{
				{Type: configs.NEWPID},
				{Type: configs.NEWNS},
				{Type: configs.NEWNET, Path: expectedNetworkPath},
				{Type: configs.NEWUTS},
				// emulate host for IPC
				//{Type: configs.NEWIPC},
			},
		},
		initProcess: &mockProcess{
			_pid:    pid,
			started: "010",
		},
	}
	container.state = &createdState{c: container}
	state, err := container.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.InitProcessPid != pid {
		t.Fatalf("expected pid %d but received %d", pid, state.InitProcessPid)
	}
	if state.InitProcessStartTime != "010" {
		t.Fatalf("expected process start time 010 but received %s", state.InitProcessStartTime)
	}
	for _, ns := range container.config.Namespaces {
		path := state.NamespacePaths[ns.Type]
		if path == "" {
			t.Fatalf("expected non nil namespace path for %s", ns.Type)
		}
		if ns.Type == configs.NEWNET {
			if path != expectedNetworkPath {
				t.Fatalf("expected path %q but received %q", expectedNetworkPath, path)
			}
		} else {
			file := ""
			switch ns.Type {
			case configs.NEWNET:
				file = "net"
			case configs.NEWNS:
				file = "mnt"
			case configs.NEWPID:
				file = "pid"
			case configs.NEWIPC:
				file = "ipc"
			case configs.NEWUSER:
				file = "user"
			case configs.NEWUTS:
				file = "uts"
			}
			expected := fmt.Sprintf("/proc/%d/ns/%s", pid, file)
			if expected != path {
				t.Fatalf("expected path %q but received %q", expected, path)
			}
		}
	}
}
