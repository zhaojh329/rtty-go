//go:build windows
// +build windows

/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package log

import (
	"github.com/rs/zerolog"
)

func newSyslogHook(_ bool) zerolog.Hook {
	return nil
}
