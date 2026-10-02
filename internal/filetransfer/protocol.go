/* SPDX-License-Identifier: MIT */

package filetransfer

import "bytes"

// Private local protocol v2. SEND and RECV carry one descriptor via SCM_RIGHTS.
const (
	Send = byte(iota + 1)
	Recv
	Accept
	Info
	Progress
	Done
	Error
)

const MaxNameLen = 255
const MaxPayloadLen = 4 + MaxNameLen

func ValidName(name []byte) bool {
	return len(name) > 0 && len(name) <= MaxNameLen && !bytes.ContainsAny(name, "\x00/") &&
		!bytes.Equal(name, []byte(".")) && !bytes.Equal(name, []byte(".."))
}
