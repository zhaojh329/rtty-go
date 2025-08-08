/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"context"
	"os"
	"runtime"
	"runtime/debug"

	xlog "github.com/zhaojh329/rtty-go/log"

	"github.com/rs/zerolog/log"
	"github.com/sevlyar/go-daemon"
	"github.com/urfave/cli/v3"
)

const RttyVersion = "1.0.0"

var (
	GitCommit = ""
	BuildTime = ""
)

func main() {
	cli.VersionFlag = &cli.BoolFlag{
		Name:        "version",
		Aliases:     []string{"V"},
		Usage:       "print the version",
		HideDefault: true,
		Local:       true,
	}

	cli.HelpFlag = &cli.BoolFlag{
		Name:        "help",
		Usage:       "show help",
		HideDefault: true,
		Local:       true,
	}

	cmd := &cli.Command{
		Name:    "rtty",
		Usage:   "Access your terminal from anywhere via the web",
		Version: RttyVersion,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "conf",
				Aliases: []string{"c"},
				Usage:   "config file to load",
			},
			&cli.StringFlag{
				Name:    "group",
				Aliases: []string{"g"},
				Usage:   "Set a group for the device(max 16 chars, no spaces allowed)",
			},
			&cli.StringFlag{
				Name:    "id",
				Aliases: []string{"I"},
				Usage:   "Set an ID for the device(max 32 chars, no spaces allowed)",
			},
			&cli.StringFlag{
				Name:    "host",
				Aliases: []string{"h"},
				Usage:   "Server's host or ipaddr(Default is localhost)",
			},
			&cli.Uint16Flag{
				Name:    "port",
				Aliases: []string{"p"},
				Usage:   "Server port(Default is 5912)",
			},
			&cli.StringFlag{
				Name:    "description",
				Aliases: []string{"d"},
				Usage:   "Add a description to the device(Maximum 126 bytes)",
			},
			&cli.BoolFlag{
				Name:    "reconnect",
				Aliases: []string{"a"},
				Usage:   "Auto reconnect to the server",
			},
			&cli.Uint8Flag{
				Name:        "heartbeat",
				Aliases:     []string{"i"},
				DefaultText: "30",
				Usage:       "Set heartbeat interval in seconds(Default is 30s)",
			},
			&cli.BoolFlag{
				Name:    "ssl",
				Aliases: []string{"s"},
				Usage:   "SSL on",
			},
			&cli.StringFlag{
				Name:    "cacert",
				Aliases: []string{"C"},
				Usage:   "CA certificate to verify peer against",
			},
			&cli.BoolFlag{
				Name:    "insecure",
				Aliases: []string{"x"},
				Usage:   "Allow insecure server connections when using SSL",
			},
			&cli.StringFlag{
				Name:    "cert",
				Aliases: []string{"c"},
				Usage:   "Certificate file to use",
			},
			&cli.StringFlag{
				Name:    "key",
				Aliases: []string{"k"},
				Usage:   "Private key file to use",
			},
			&cli.BoolFlag{
				Name:  "D",
				Usage: "Run in the background",
			},
			&cli.StringFlag{
				Name:    "token",
				Aliases: []string{"t"},
				Usage:   "Authorization token",
			},
			&cli.BoolFlag{
				Name:  "R",
				Usage: "Receive file",
			},
			&cli.StringFlag{
				Name:  "S",
				Usage: "Send file",
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "verbose",
			},
		},
		Action: cmdAction,
	}

	if runtime.GOOS != "windows" {
		cmd.Flags = append(cmd.Flags, &cli.StringFlag{
			Name:    "username",
			Aliases: []string{"f"},
			Usage:   "Skip a second login authentication. See man login(1) about the details",
		})
	}

	err := cmd.Run(context.Background(), os.Args)
	if err != nil {
		log.Fatal().Msg(err.Error())
	}
}

func cmdAction(c context.Context, cmd *cli.Command) error {
	defer logPanic()

	if cmd.Bool("R") {
		requestTransferFile('R', "")
		return nil
	}

	if cmd.IsSet("S") {
		requestTransferFile('S', cmd.String("S"))
		return nil
	}

	cfg := Config{
		host:      "localhost",
		heartbeat: 30,
		port:      5912,
	}

	err := cfg.Parse(cmd)
	if err != nil {
		return err
	}

	if cmd.Bool("D") {
		context := &daemon.Context{}

		d, err := context.Reborn()
		if err != nil {
			return err
		}

		if d != nil {
			return nil
		}

		defer context.Release()
	}

	xlog.LogInit(cmd.Bool("verbose"))

	if runtime.GOOS != "windows" {
		go signalHandle()
	}

	log.Info().Msg("Go Version: " + runtime.Version())
	log.Info().Msgf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH)

	log.Info().Msg("Rtty Version: " + RttyVersion)

	if GitCommit != "" {
		log.Info().Msg("Git Commit: " + GitCommit)
	}

	if BuildTime != "" {
		log.Info().Msg("Build Time: " + BuildTime)
	}

	log.Debug().Msgf("%+v", cfg)

	rtty := &RttyClient{cfg: cfg}

	rtty.Run()

	return nil
}

func logPanic() {
	if r := recover(); r != nil {
		saveCrashLog(r, debug.Stack())
		os.Exit(2)
	}
}

func saveCrashLog(p any, stack []byte) {
	log.Error().Msgf("%v", p)
	log.Error().Msg(string(stack))
}
