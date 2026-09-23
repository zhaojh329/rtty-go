package filetransfer

import (
	"encoding/binary"
	"testing"
)

func TestLocalTransferWireValues(t *testing.T) {
	magic := NewMagic('S', 0x12345678, 9)
	if !IsMagic(magic[:]) || magic[3] != 'S' || binary.NativeEndian.Uint32(magic[4:]) != 0x12345678 || binary.NativeEndian.Uint32(magic[8:]) != 9 {
		t.Errorf("invalid transfer magic: %v", magic)
	}

	wrongPrefix := magic
	wrongPrefix[0] = 0
	if IsMagic(wrongPrefix[:]) || IsMagic(magic[:11]) {
		t.Error("accepted an invalid transfer magic")
	}

	if ControlMessageSize != 129 {
		t.Errorf("control message size = %d", ControlMessageSize)
	}

	values := []byte{
		ControlRequestAccept, ControlProgress, ControlInfo, ControlBusy,
		ControlAbort, ControlNoSpace, ControlErrExist, ControlErr,
	}
	for i, value := range values {
		if value != byte(i) {
			t.Errorf("control type %d = %d", i, value)
		}
	}
}
