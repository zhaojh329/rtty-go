package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
	"github.com/zhaojh329/rtty-go/internal/client"
)

func TestParseConfig(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "rtty.conf")
	if err := os.WriteFile(conf, []byte("id: configured\nhost: example.net\nport: 7000\n"), 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		args    []string
		wantID  string
		wantErr string
	}{
		{name: "missing ID", args: []string{"rtty"}, wantErr: "you must specify an id"},
		{name: "invalid group", args: []string{"rtty", "--id", "device", "--group", "bad group"}, wantID: "device", wantErr: "invalid group"},
		{name: "flag overrides config", args: []string{"rtty", "--conf", conf, "--id", "bad id"}, wantID: "bad id", wantErr: "invalid device id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg client.Config
			var parseErr error
			cmd := &cli.Command{
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "conf"},
					&cli.StringFlag{Name: "id"},
					&cli.StringFlag{Name: "group"},
					&cli.StringFlag{Name: "host"},
					&cli.Uint16Flag{Name: "port"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					cfg, parseErr = parseConfig(c)
					return nil
				},
			}

			if err := cmd.Run(context.Background(), tt.args); err != nil {
				t.Fatal(err)
			}

			if parseErr == nil || !strings.Contains(parseErr.Error(), tt.wantErr) {
				t.Fatalf("parseConfig() error = %v, want %q", parseErr, tt.wantErr)
			}
			if cfg.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", cfg.ID, tt.wantID)
			}
			if tt.name == "missing ID" && (cfg.Host != "localhost" || cfg.Port != 5912 || cfg.Heartbeat != 30) {
				t.Errorf("defaults changed: Host = %q, Port = %d, Heartbeat = %d", cfg.Host, cfg.Port, cfg.Heartbeat)
			}
			if tt.name == "flag overrides config" && (cfg.Host != "example.net" || cfg.Port != 7000) {
				t.Errorf("config file values lost: Host = %q, Port = %d", cfg.Host, cfg.Port)
			}
		})
	}
}

func TestHTTPTimeoutConfig(t *testing.T) {
	if os.Getuid() > 0 {
		t.Skip("config parsing currently requires root")
	}

	tests := []struct {
		name string
		yaml string
		args []string
		want int
	}{
		{name: "default", want: 30},
		{name: "flag", args: []string{"--http-timeout", "60"}, want: 60},
		{name: "minimum", args: []string{"--http-timeout", "5"}, want: 5},
		{name: "maximum", args: []string{"--http-timeout", "255"}, want: 255},
		{name: "negative", args: []string{"--http-timeout", "-1"}, want: 5},
		{name: "zero", args: []string{"--http-timeout", "0"}, want: 5},
		{name: "too large", args: []string{"--http-timeout", "256"}, want: 255},
		{name: "yaml", yaml: "http-timeout: 45\n", want: 45},
		{name: "yaml too small", yaml: "http-timeout: 4\n", want: 5},
		{name: "yaml too large", yaml: "http-timeout: 256\n", want: 255},
		{name: "flag overrides yaml", yaml: "http-timeout: 45\n", args: []string{"--http-timeout", "90"}, want: 90},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := filepath.Join(t.TempDir(), "rtty.conf")
			if err := os.WriteFile(conf, []byte("id: device\n"+tt.yaml), 0600); err != nil {
				t.Fatal(err)
			}

			cmd := &cli.Command{
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "conf"},
					&cli.IntFlag{Name: "http-timeout"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					cfg, err := parseConfig(c)
					if err != nil {
						return err
					}

					if cfg.HTTPTimeout != tt.want {
						t.Errorf("HTTPTimeout = %d, want %d", cfg.HTTPTimeout, tt.want)
					}

					return nil
				},
			}

			args := append([]string{"rtty", "--conf", conf}, tt.args...)
			if err := cmd.Run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
		})
	}
}
