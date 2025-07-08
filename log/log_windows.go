//go:build windows
// +build windows

package log

import (
	"github.com/rs/zerolog"
)

func newSyslogHook(_ bool) zerolog.Hook {
	return nil
}
