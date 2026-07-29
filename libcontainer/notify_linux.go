//go:build linux
// +build linux

package libcontainer

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"syscall"
)

const oomCgroupName = "memory"

type PressureLevel uint

const (
	LowPressure PressureLevel = iota
	MediumPressure
	CriticalPressure
)

func registerMemoryEvent(cgDir string, evName string, arg string) (<-chan struct{}, error) {
	evFile, err := os.Open(filepath.Join(cgDir, evName))
	if err != nil {
		return nil, err
	}
	fd, _, syserr := syscall.RawSyscall(syscall.SYS_EVENTFD2, 0, syscall.FD_CLOEXEC, 0)
	if syserr != 0 {
		evFile.Close()
		return nil, syserr
	}

	eventfd := os.NewFile(fd, "eventfd")

	eventControlPath := filepath.Join(cgDir, "cgroup.event_control")
	data := fmt.Sprintf("%d %d %s", eventfd.Fd(), evFile.Fd(), arg)
	if err := ioutil.WriteFile(eventControlPath, []byte(data), 0700); err != nil {
		eventfd.Close()
		evFile.Close()
		return nil, err
	}
	ch := make(chan struct{})
	go func() {
		defer func() {
			close(ch)
			eventfd.Close()
			evFile.Close()
		}()
		buf := make([]byte, 8)
		for {
			if _, err := eventfd.Read(buf); err != nil {
				return
			}
			// When a cgroup is destroyed, an event is sent to eventfd.
			// So if the control path is gone, return instead of notifying.
			if _, err := os.Lstat(eventControlPath); os.IsNotExist(err) {
				return
			}
			ch <- struct{}{}
		}
	}()
	return ch, nil
}
