/* SPDX-License-Identifier: MIT */

package client

type Config struct {
	Group       string
	ID          string
	Host        string
	Port        uint16
	Description string
	Token       string
	Heartbeat   uint8
	Username    string
	Reconnect   bool
	SSL         bool
	CACert      string
	SSLCert     string
	SSLKey      string
	Insecure    bool
}
