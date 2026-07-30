//go:build linux
// +build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/urfave/cli"
)

// event struct for encoding the event data to json.
type event struct {
	Type string      `json:"type"`
	ID   string      `json:"id"`
	Data interface{} `json:"data,omitempty"`
}

// stats is the runc specific stats structure for stability when encoding and decoding stats.
// This build of runc has no cgroups dependency, so only network interface
// statistics are available; there is no resource usage data to report.
type stats struct {
	Interfaces []*libcontainer.NetworkInterface `json:"interfaces,omitempty"`
}

var eventsCommand = cli.Command{
	Name:  "events",
	Usage: "display container events such as network interface statistics",
	ArgsUsage: `<container-id>

Where "<container-id>" is the name for the instance of the container.`,
	Description: `The events command displays information about the container. By default the
information is displayed once every 5 seconds.`,
	Flags: []cli.Flag{
		cli.DurationFlag{Name: "interval", Value: 5 * time.Second, Usage: "set the stats collection interval"},
		cli.BoolFlag{Name: "stats", Usage: "display the container's stats then exit"},
	},
	Action: func(context *cli.Context) error {
		if err := checkArgs(context, 1, exactArgs); err != nil {
			return err
		}
		container, err := getContainer(context)
		if err != nil {
			return err
		}
		duration := context.Duration("interval")
		if duration <= 0 {
			return fmt.Errorf("duration interval must be greater than 0")
		}
		status, err := container.Status()
		if err != nil {
			return err
		}
		if status == libcontainer.Stopped {
			return fmt.Errorf("container with id %s is not running", container.ID())
		}
		var (
			statsCh = make(chan *libcontainer.Stats, 1)
			events  = make(chan *event, 1024)
			group   = &sync.WaitGroup{}
		)
		group.Add(1)
		go func() {
			defer group.Done()
			enc := json.NewEncoder(os.Stdout)
			for e := range events {
				if err := enc.Encode(e); err != nil {
					logrus.Error(err)
				}
			}
		}()
		if context.Bool("stats") {
			s, err := container.Stats()
			if err != nil {
				return err
			}
			events <- &event{Type: "stats", ID: container.ID(), Data: convertLibcontainerStats(s)}
			close(events)
			group.Wait()
			return nil
		}
		go func() {
			for range time.Tick(context.Duration("interval")) {
				s, err := container.Stats()
				if err != nil {
					logrus.Error(err)
					continue
				}
				statsCh <- s
			}
		}()
		for s := range statsCh {
			events <- &event{Type: "stats", ID: container.ID(), Data: convertLibcontainerStats(s)}
		}
		group.Wait()
		return nil
	},
}

func convertLibcontainerStats(ls *libcontainer.Stats) *stats {
	return &stats{Interfaces: ls.Interfaces}
}
