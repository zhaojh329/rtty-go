/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package proto

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/valyala/bytebufferpool"
)

type Role int

const (
	RoleRtty = Role(iota)
	RoleRttys
)

const (
	MsgTypeRegister = byte(iota)
	MsgTypeLogin
	MsgTypeLogout
	MsgTypeTermData
	MsgTypeWinsize
	MsgTypeCmd
	MsgTypeHeartbeat
	MsgTypeFile
	MsgTypeHttp
	MsgTypeAck
	MsgTypeSerialPorts
	MsgTypeSerialOpen
)

const (
	SerialOK = byte(iota)
	SerialBusy
	SerialNotFound
	SerialPermission
	SerialInvalidSettings
	SerialFailed
)

type SerialParity byte

const (
	SerialParityNone SerialParity = iota
	SerialParityOdd
	SerialParityEven
)

type SerialSettings struct {
	Port     string
	BaudRate int
	DataBits int
	StopBits int
	Parity   SerialParity
}

func (s SerialSettings) MarshalBinary() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("invalid serial settings")
	}

	data := make([]byte, 7, 7+len(s.Port))
	binary.BigEndian.PutUint32(data[:4], uint32(s.BaudRate))
	data[4] = byte(s.DataBits)
	data[5] = byte(s.StopBits)
	data[6] = byte(s.Parity)
	data = append(data, s.Port...)

	return data, nil
}

func ParseSerialSettings(data []byte) (SerialSettings, error) {
	if len(data) < 8 || len(data) > 263 {
		return SerialSettings{}, fmt.Errorf("invalid serial settings length")
	}

	baud := binary.BigEndian.Uint32(data[:4])
	if baud > 4000000 {
		return SerialSettings{}, fmt.Errorf("invalid serial baud rate")
	}

	settings := SerialSettings{
		Port: string(data[7:]), BaudRate: int(baud),
		DataBits: int(data[4]), StopBits: int(data[5]), Parity: SerialParity(data[6]),
	}

	if settings.Parity > SerialParityEven {
		return SerialSettings{}, fmt.Errorf("invalid serial parity")
	}

	if !settings.Valid() {
		return settings, fmt.Errorf("invalid serial settings")
	}

	return settings, nil
}

func (s SerialSettings) Valid() bool {
	return s.Port != "" && len(s.Port) <= 256 &&
		s.BaudRate >= 300 && s.BaudRate <= 4000000 &&
		s.DataBits >= 5 && s.DataBits <= 8 &&
		(s.StopBits == 1 || s.StopBits == 2) &&
		s.Parity <= SerialParityEven
}

const (
	MsgRegAttrHeartbeat = byte(iota)
	MsgRegAttrDevid
	MsgRegAttrDescription
	MsgRegAttrToken
	MsgRegAttrGroup
)

const (
	MsgHeartbeatAttrUptime = byte(iota)
)

const (
	MsgTypeFileSend = byte(iota)
	MsgTypeFileRecv
	MsgTypeFileInfo
	MsgTypeFileData
	MsgTypeFileAck
	MsgTypeFileAbort
)

const (
	MaximumDevIDLen = 32
	MaximumGroupLen = 16
	MaximumDescLen  = 126
)

var minimumMsgLensRtty = map[byte]int{
	MsgTypeRegister:    1,
	MsgTypeLogin:       32,
	MsgTypeLogout:      32,
	MsgTypeTermData:    33,
	MsgTypeWinsize:     36,
	MsgTypeFile:        33,
	MsgTypeAck:         34,
	MsgTypeHttp:        25,
	MsgTypeSerialPorts: 32,
	MsgTypeSerialOpen:  40,
}

var minimumMsgLensRttys = map[byte]int{
	MsgTypeRegister:    1,
	MsgTypeLogin:       33,
	MsgTypeLogout:      32,
	MsgTypeTermData:    33,
	MsgTypeFile:        33,
	MsgTypeHttp:        18,
	MsgTypeSerialPorts: 33,
	MsgTypeSerialOpen:  33,
}

func MsgTypeName(typ byte) string {
	switch typ {
	case MsgTypeRegister:
		return "register"
	case MsgTypeLogin:
		return "login"
	case MsgTypeLogout:
		return "logout"
	case MsgTypeTermData:
		return "termdata"
	case MsgTypeWinsize:
		return "winsize"
	case MsgTypeCmd:
		return "cmd"
	case MsgTypeHeartbeat:
		return "heartbeat"
	case MsgTypeFile:
		return "file"
	case MsgTypeHttp:
		return "http"
	case MsgTypeAck:
		return "ack"
	case MsgTypeSerialPorts:
		return "serialports"
	case MsgTypeSerialOpen:
		return "serialopen"
	default:
		return fmt.Sprintf("unknown(%d)", typ)
	}
}

func NewMsgReaderWriter(role Role, conn net.Conn) *MsgReaderWriter {
	msg := &MsgReaderWriter{
		conn: conn,
		br:   bufio.NewReader(conn),
	}

	if role == RoleRtty {
		msg.minimumMsgLens = minimumMsgLensRtty
	} else {
		msg.minimumMsgLens = minimumMsgLensRttys
	}

	return msg
}

type MsgReaderWriter struct {
	minimumMsgLens map[byte]int

	conn net.Conn
	br   *bufio.Reader
	head [3]byte
	buf  []byte
}

func (msg *MsgReaderWriter) Read() (byte, []byte, error) {
	head := msg.head
	br := msg.br

	_, err := io.ReadFull(br, head[:])
	if err != nil {
		return 0, nil, err
	}

	typ := head[0]
	msgLen := binary.BigEndian.Uint16(head[1:])

	if fixedMsgLen, ok := msg.minimumMsgLens[typ]; ok {
		if msgLen < uint16(fixedMsgLen) {
			return 0, nil, fmt.Errorf("invalid message length for %s: at least %d, got %d",
				MsgTypeName(typ), fixedMsgLen, msgLen)
		}
	}

	if cap(msg.buf) < int(msgLen) {
		msg.buf = make([]byte, msgLen)
	} else {
		msg.buf = msg.buf[:msgLen]
	}

	_, err = io.ReadFull(br, msg.buf)
	if err != nil {
		return 0, nil, err
	}

	return typ, msg.buf, nil
}

func (msg *MsgReaderWriter) Write(typ byte, data ...any) error {
	bb := bytebufferpool.Get()
	defer bytebufferpool.Put(bb)

	bb.WriteByte(typ)

	// 2 bytes placeholder
	bb.WriteByte(0)
	bb.WriteByte(0)

	total := 0

	for _, d := range data {
		length := 0

		switch v := d.(type) {
		case uint16:
			bb.WriteByte(0)
			bb.WriteByte(0)
			length = 2
			binary.BigEndian.PutUint16(bb.B[bb.Len()-2:], v)
		case uint32:
			bb.WriteByte(0)
			bb.WriteByte(0)
			bb.WriteByte(0)
			bb.WriteByte(0)
			length = 4
			binary.BigEndian.PutUint32(bb.B[bb.Len()-4:], v)
		case byte:
			bb.WriteByte(v)
			length = 1
		case []byte:
			length, _ = bb.Write(v)
		case string:
			length, _ = bb.WriteString(v)
		case *bytebufferpool.ByteBuffer:
			length, _ = bb.Write(v.B)
		default:
			return fmt.Errorf("unsupported data type: %T", v)
		}

		total += length
	}

	if total > 0xffff {
		return fmt.Errorf("data too long, exceeds 0xffff")
	}

	binary.BigEndian.PutUint16(bb.B[1:], uint16(total))

	_, err := bb.WriteTo(msg.conn)

	return err
}
