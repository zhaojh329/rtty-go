/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package log

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/dwdcth/consoleEx"
	"github.com/mattn/go-colorable"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/term"
)

func LogInit(debug bool) {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		hook := newSyslogHook(debug)
		if hook != nil {
			log.Logger = log.Logger.Hook(hook)
		}
	}

	if debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
}

func init() {
	zerolog.CallerMarshalFunc = func(pc uintptr, file string, line int) string {
		return filepath.Base(file) + ":" + strconv.Itoa(line)
	}

	out := consoleEx.ConsoleWriterEx{Out: colorable.NewColorableStdout()}
	logger := zerolog.New(out).With().Timestamp().Logger().With().Caller().Logger()

	log.Logger = logger

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
}
