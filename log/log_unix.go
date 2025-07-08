//go:build !windows
// +build !windows

package log

import (
	"log/syslog"
	"runtime"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type syslogHook struct {
	sysLog *syslog.Writer
}

func (h *syslogHook) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	var caller string

	pc, file, line, ok := runtime.Caller(3)
	if ok {
		caller = zerolog.CallerMarshalFunc(pc, file, line) + " |"
	}

	msg = caller + msg

	switch level {
	case zerolog.DebugLevel:
		h.sysLog.Debug(msg)
	case zerolog.InfoLevel:
		h.sysLog.Info(msg)
	case zerolog.WarnLevel:
		h.sysLog.Warning(msg)
	case zerolog.ErrorLevel:
		h.sysLog.Err(msg)
	}
}

func newSyslogHook(debug bool) zerolog.Hook {
	var priority syslog.Priority
	if debug {
		priority = syslog.LOG_DEBUG
	} else {
		priority = syslog.LOG_INFO
	}
	sysLog, err := syslog.New(priority, "rtty")
	if err != nil {
		log.Fatal().Msg(err.Error())
	}
	return &syslogHook{sysLog}
}
