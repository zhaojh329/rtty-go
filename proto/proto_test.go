package proto

import (
	"bytes"
	"net"
	"testing"
)

func TestSerialSettingsBinary(t *testing.T) {
	settings := SerialSettings{Port: "COM3", BaudRate: 115200, DataBits: 8, StopBits: 1, Parity: SerialParityNone}
	data, err := settings.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 1, 0xc2, 0, 8, 1, 0, 'C', 'O', 'M', '3'}
	if !bytes.Equal(data, want) {
		t.Fatalf("serial bytes: got %v, want %v", data, want)
	}
	got, err := ParseSerialSettings(data)
	if err != nil || got != settings {
		t.Fatalf("round trip: got %+v, error %v", got, err)
	}
	if _, err := (SerialSettings{}).MarshalBinary(); err == nil {
		t.Fatal("encoded invalid settings")
	}

	bad := map[string][]byte{
		"short":          data[:6],
		"missing port":   data[:7],
		"invalid baud":   append([]byte{0, 0, 0, 0}, data[4:]...),
		"invalid parity": append(bytes.Clone(data[:6]), append([]byte{3}, data[7:]...)...),
		"long port":      append(bytes.Clone(data[:7]), bytes.Repeat([]byte{'x'}, 257)...),
	}
	for name, payload := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSerialSettings(payload); err == nil {
				t.Fatal("accepted invalid serial settings")
			}
		})
	}
	for parity, code := range map[SerialParity]byte{SerialParityOdd: 1, SerialParityEven: 2} {
		settings.Parity = parity
		encoded, err := settings.MarshalBinary()
		if err != nil || encoded[6] != code {
			t.Fatalf("encode parity %v: %v, %v", parity, encoded, err)
		}
		decoded, err := ParseSerialSettings(encoded)
		if err != nil || decoded != settings {
			t.Fatalf("decode parity %v: %+v, %v", parity, decoded, err)
		}
	}
}

func TestSerialSettingsValid(t *testing.T) {
	settings := SerialSettings{Port: "COM3", BaudRate: 115200, DataBits: 8, StopBits: 1, Parity: SerialParityNone}
	if !settings.Valid() {
		t.Fatal("default serial settings should be valid")
	}

	tests := []SerialSettings{
		{Port: "", BaudRate: 115200, DataBits: 8, StopBits: 1, Parity: SerialParityNone},
		{Port: "COM3", BaudRate: 299, DataBits: 8, StopBits: 1, Parity: SerialParityNone},
		{Port: "COM3", BaudRate: 115200, DataBits: 9, StopBits: 1, Parity: SerialParityNone},
		{Port: "COM3", BaudRate: 115200, DataBits: 8, StopBits: 3, Parity: SerialParityNone},
		{Port: "COM3", BaudRate: 115200, DataBits: 8, StopBits: 1, Parity: SerialParity(3)},
	}
	for _, settings := range tests {
		if settings.Valid() {
			t.Fatalf("accepted invalid settings: %+v", settings)
		}
	}
}

func TestSerialMessageLength(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	request := NewMsgReaderWriter(RoleRtty, clientConn)
	response := NewMsgReaderWriter(RoleRttys, serverConn)
	id := "0123456789abcdef0123456789abcdef"
	done := make(chan error, 1)
	go func() { done <- response.Write(MsgTypeSerialPorts, id, SerialOK, []byte(`[]`)) }()

	message, body, err := request.Read()
	if err != nil || message != MsgTypeSerialPorts || len(body) != 35 {
		t.Fatalf("read serial response: type=%d body=%q error=%v", message, body, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
