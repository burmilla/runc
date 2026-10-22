//go:build linux
// +build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/urfave/cli"
)

// containerd calls `ps --format=json` from its State API, so only json is kept.
var psCommand = cli.Command{
	Name:      "ps",
	Usage:     "ps displays the processes running inside a container",
	ArgsUsage: `<container-id>`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "format, f",
			Value: "json",
			Usage: `output format, only json is supported`,
		},
	},
	Action: func(context *cli.Context) error {
		if err := checkArgs(context, 1, exactArgs); err != nil {
			return err
		}
		if context.String("format") != "json" {
			return fmt.Errorf("invalid format option")
		}
		container, err := getContainer(context)
		if err != nil {
			return err
		}
		pids, err := container.Processes()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(pids)
	},
}
