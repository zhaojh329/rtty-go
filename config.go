/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/kylelemons/go-gypsy/yaml"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
	"github.com/zhaojh329/rtty-go/proto"
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

	KCP            bool
	KcpNodelay     bool
	KcpInterval    int
	KcpResend      int
	KcpNc          bool
	KcpSndwnd      int
	KcpRcvwnd      int
	KcpMtu         int
	KcpPassword    string
	KcpDataShard   int
	KcpParityShard int
}

func (cfg *Config) Parse(c *cli.Command) error {
	var yamlCfg *yaml.File
	var err error

	conf := c.String("conf")
	if conf != "" {
		yamlCfg, err = yaml.ReadFile(conf)
		if err != nil {
			return fmt.Errorf(`read config file: %s`, err.Error())
		}
	}

	fields := map[string]any{
		"group":       &cfg.group,
		"id":          &cfg.id,
		"host":        &cfg.host,
		"port":        &cfg.port,
		"description": &cfg.description,
		"token":       &cfg.token,
		"heartbeat":   &cfg.heartbeat,
		"username":    &cfg.username,
		"reconnect":   &cfg.reconnect,
		"ssl":         &cfg.ssl,
		"cacert":      &cfg.cacert,
		"cert":        &cfg.sslcert,
		"key":         &cfg.sslkey,
		"insecure":    &cfg.insecure,

		"kcp":              &cfg.KCP,
		"kcp-nodelay":      &cfg.KcpNodelay,
		"kcp-interval":     &cfg.KcpInterval,
		"kcp-resend":       &cfg.KcpResend,
		"kcp-nc":           &cfg.KcpNc,
		"kcp-sndwnd":       &cfg.KcpSndwnd,
		"kcp-rcvwnd":       &cfg.KcpRcvwnd,
		"kcp-mtu":          &cfg.KcpMtu,
		"kcp-key":          &cfg.KcpPassword,
		"kcp-data-shard":   &cfg.KcpDataShard,
		"kcp-parity-shard": &cfg.KcpParityShard,
	}

	for name, opt := range fields {
		if yamlCfg != nil {
			if err := getConfigOpt(yamlCfg, name, opt); err != nil {
				return err
			}
		}
		getFlagOpt(c, name, opt)
	}

	getFlagOpt(c, "f", &cfg.username)
	getFlagOpt(c, "a", &cfg.reconnect)

	if cfg.id == "" {
		return fmt.Errorf("you must specify an id for your device")
	}

	if strings.ContainsAny(cfg.id, " ") || len(cfg.id) > proto.MaximumDevIDLen {
		return fmt.Errorf("invalid device id: must be 1-32 characters and cannot contain spaces")
	}

	if strings.ContainsAny(cfg.group, " ") || len(cfg.group) > proto.MaximumGroupLen {
		return fmt.Errorf("invalid group: must be 1-16 characters and cannot contain spaces")
	}

	if len(cfg.description) > proto.MaximumDescLen {
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

func getConfigOpt(yamlCfg *yaml.File, name string, opt any) error {
	var num int64
	var err error

	switch opt := opt.(type) {
	case *string:
		var val string
		val, err = yamlCfg.Get(name)
		if err == nil {
			*opt = val
		}
	case *bool:
		var val bool
		val, err = yamlCfg.GetBool(name)
		if err == nil {
			*opt = val
		}
	case *int, *uint, *uint8, *uint16:
		num, err = yamlCfg.GetInt(name)
		if err == nil {
			switch opt := opt.(type) {
			case *int:
				*opt = int(num)
			case *uint:
				*opt = uint(num)
			case *uint8:
				*opt = uint8(num)
			case *uint16:
				*opt = uint16(num)
			}
		}
	default:
		return fmt.Errorf("unsupported type for option %s", name)
	}

	if err != nil {
		if _, ok := err.(*yaml.NodeNotFound); ok {
			return nil
		}
		return fmt.Errorf(`invalud "%s": %w`, name, err)
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
