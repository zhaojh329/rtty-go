//go:build !windows

package client

import (
	"testing"

	"github.com/zhaojh329/rtty-go/proto"
)

func TestFileMsgIgnoresSerialSession(t *testing.T) {
	cli := New(Config{})
	id := "0123456789abcdef0123456789abcdef"
	cli.sessions.Store(id, &SerialSession{})

	if err := handleFileMsg(cli, append([]byte(id), proto.MsgTypeFileAck)); err != nil {
		t.Fatal(err)
	}
}
