/*
 * MIT License
 *
 * Copyright (c) 2025 Jianhui Zhao <zhaojh329@gmail.com>
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
)

type Config struct {
	group       string
	id          string
	host        string
	port        uint16
	description string
	token       string
	heartbeat   uint8
	username    string
	reconnect   bool

	ssl      bool
	cacert   string
	sslcert  string
	sslkey   string
	insecure bool
}

func (cfg *Config) Parse(c *cli.Command) error {
	getFlagOpt(c, "group", &cfg.group)
	getFlagOpt(c, "id", &cfg.id)
	getFlagOpt(c, "host", &cfg.host)
	getFlagOpt(c, "port", &cfg.port)
	getFlagOpt(c, "description", &cfg.description)
	getFlagOpt(c, "token", &cfg.token)
	getFlagOpt(c, "i", &cfg.heartbeat)
	getFlagOpt(c, "f", &cfg.username)
	getFlagOpt(c, "a", &cfg.reconnect)
	getFlagOpt(c, "s", &cfg.ssl)
	getFlagOpt(c, "cacert", &cfg.cacert)
	getFlagOpt(c, "cert", &cfg.sslcert)
	getFlagOpt(c, "key", &cfg.sslkey)
	getFlagOpt(c, "insecure", &cfg.insecure)

	if cfg.id == "" {
		return fmt.Errorf("you must specify an id for your device")
	}

	if strings.ContainsAny(cfg.id, " ") || len(cfg.id) > 32 {
		return fmt.Errorf("invalid device id: must be 1-32 characters and cannot contain spaces")
	}

	if strings.ContainsAny(cfg.group, " ") || len(cfg.group) > 16 {
		return fmt.Errorf("invalid group: must be 1-16 characters and cannot contain spaces")
	}

	if len(cfg.description) > 126 {
		return fmt.Errorf("description too long: must be 1-126 characters")
	}

	if cfg.heartbeat < 5 {
		cfg.heartbeat = 5
		log.Warn().Msgf("heartbeat interval too low, setting to minimum 5 seconds")
	}

	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		return fmt.Errorf("operation not permitted, must be run as root")
	}

	return nil
}

func getFlagOpt(c *cli.Command, name string, opt any) {
	if !c.IsSet(name) {
		return
	}

	switch opt := opt.(type) {
	case *string:
		*opt = c.String(name)
	case *int:
		*opt = c.Int(name)
	case *uint:
		*opt = c.Uint(name)
	case *uint8:
		*opt = c.Uint8(name)
	case *uint16:
		*opt = c.Uint16(name)
	case *bool:
		*opt = c.Bool(name)
	}
}
