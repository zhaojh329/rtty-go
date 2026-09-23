/* SPDX-License-Identifier: MIT */

package filetransfer

import "encoding/binary"

const (
	ControlRequestAccept = byte(iota)
	ControlProgress
	ControlInfo
	ControlBusy
	ControlAbort
	ControlNoSpace
	ControlErrExist
	ControlErr
)

const ControlMessageSize = 129

var magicPrefix = [3]byte{0xb6, 0xbc, 0xbd}

func NewMagic(typ byte, pid uint32, fd uint32) [12]byte {
	var magic [12]byte
	copy(magic[:], magicPrefix[:])
	magic[3] = typ
	binary.NativeEndian.PutUint32(magic[4:], pid)
	binary.NativeEndian.PutUint32(magic[8:], fd)

	return magic
}

func IsMagic(data []byte) bool {
	return len(data) == 12 && data[0] == magicPrefix[0] && data[1] == magicPrefix[1] && data[2] == magicPrefix[2]
}
