/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import "github.com/zhaojh329/rtty-go/proto"

type DataPlaneTransport interface {
	Mode() string
	SendTermData(sid string, data []byte) (bool, error)
	SendFile(sid string, typ byte, data []byte) error
	SendHTTP(saddr [18]byte, data []byte) error
}

type relayDataTransport struct {
	cli *RttyClient
}

func newRelayDataTransport(cli *RttyClient) DataPlaneTransport {
	return &relayDataTransport{cli: cli}
}

func (t *relayDataTransport) Mode() string {
	return "server-relay"
}

func (t *relayDataTransport) SendTermData(sid string, data []byte) (bool, error) {
	if sent, err := t.cli.peerSignals().SendTerminalData(sid, data); sent || err != nil {
		return false, err
	}

	return true, t.cli.WriteMsg(proto.MsgTypeTermData, sid, data)
}

func (t *relayDataTransport) SendFile(sid string, typ byte, data []byte) error {
	return t.cli.WriteMsg(proto.MsgTypeFile, sid, typ, data)
}

func (t *relayDataTransport) SendHTTP(saddr [18]byte, data []byte) error {
	return t.cli.WriteMsg(proto.MsgTypeHttp, saddr[:], data)
}
