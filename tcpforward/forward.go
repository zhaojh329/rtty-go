// SPDX-License-Identifier: MIT
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

// Package tcpforward bridges a TCP socket and rtty TCP protocol messages with
// independent, bounded byte windows in both directions.
package tcpforward

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/zhaojh329/rtty-go/proto"
)

// Forwarder owns one socket. Handle may run concurrently with Run and Close.
// The send callback must serialize protocol writes and must not retain data.
type Forwarder struct {
	conn net.Conn
	send func(op byte, data []byte) error

	mu       sync.Mutex
	cond     *sync.Cond
	closed   bool
	unacked  int
	input    []byte
	head     int
	buffered int
	pending  int // Includes data currently being written to the socket.
	half     bool
}

func New(conn net.Conn, send func(byte, []byte) error) *Forwarder {
	f := &Forwarder{conn: conn, send: send}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Handle accepts Data, CloseWrite and Ack without waiting for socket I/O.
// An error indicates a protocol violation; the owner should abort this forward.
func (f *Forwarder) Handle(op byte, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil
	}

	switch op {
	case proto.TCPTypeData:
		if len(data) == 0 || len(data) > proto.TCPMaxDataSize || f.half {
			return fmt.Errorf("invalid TCP data")
		}

		if len(data) > proto.TCPWindowSize-f.pending {
			return fmt.Errorf("TCP receive window exceeded")
		}

		if f.input == nil {
			f.input = make([]byte, proto.TCPWindowSize)
		}

		tail := (f.head + f.buffered) % len(f.input)
		n := copy(f.input[tail:], data)
		copy(f.input, data[n:])
		f.buffered += len(data)
		f.pending += len(data)

	case proto.TCPTypeCloseWrite:
		if len(data) != 0 || f.half {
			return fmt.Errorf("invalid TCP half close")
		}

		f.half = true

	case proto.TCPTypeAck:
		if len(data) != 4 {
			return fmt.Errorf("invalid TCP acknowledgement")
		}

		n := binary.BigEndian.Uint32(data)
		if n == 0 || uint64(n) > uint64(f.unacked) {
			return fmt.Errorf("TCP acknowledgement exceeds outstanding data")
		}

		f.unacked -= int(n)

	default:
		return fmt.Errorf("invalid TCP operation")
	}

	f.cond.Broadcast()
	return nil
}

// Close aborts socket I/O and wakes all window/queue waiters.
func (f *Forwarder) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}

	f.closed = true
	f.cond.Broadcast()
	f.mu.Unlock()
	f.conn.Close()
}

// Run forwards until both directions finish or an error aborts the socket.
// A nil result means an orderly close: no TCPTypeClose should be sent, since the
// remote forward may still be draining data preceding our CloseWrite message.
// The owner must close the protocol connection to cancel a blocked send callback.
func (f *Forwarder) Run() error {
	defer f.Close()

	written := make(chan error, 1)
	go func() {
		err := f.writeLoop()
		if err != nil {
			f.Close()
		}

		written <- err
	}()

	err := f.readLoop()
	if err != nil {
		f.Close()
	}

	writeErr := <-written
	if err != nil {
		return err
	}

	return writeErr
}

func (f *Forwarder) readLoop() error {
	buf := make([]byte, proto.TCPMaxDataSize)
	for {
		f.mu.Lock()
		for f.unacked == proto.TCPWindowSize && !f.closed {
			f.cond.Wait()
		}

		if f.closed {
			f.mu.Unlock()
			return net.ErrClosed
		}

		available := min(len(buf), proto.TCPWindowSize-f.unacked)
		f.mu.Unlock()

		n, err := f.conn.Read(buf[:available])
		if n > 0 {
			// Reserve credit before sending: the peer may ACK before send returns.
			f.mu.Lock()
			f.unacked += n
			f.mu.Unlock()

			if sendErr := f.send(proto.TCPTypeData, buf[:n]); sendErr != nil {
				return sendErr
			}
		}

		if err == io.EOF {
			return f.send(proto.TCPTypeCloseWrite, nil)
		}

		if err != nil {
			return err
		}
	}
}

func (f *Forwarder) writeLoop() error {
	buf := make([]byte, proto.TCPMaxDataSize)
	for {
		f.mu.Lock()
		for f.buffered == 0 && !f.half && !f.closed {
			f.cond.Wait()
		}

		if f.closed {
			f.mu.Unlock()
			return net.ErrClosed
		}

		if f.buffered == 0 {
			f.mu.Unlock()
			conn, ok := f.conn.(interface{ CloseWrite() error })
			if !ok {
				return fmt.Errorf("TCP socket does not support half close")
			}

			return conn.CloseWrite()
		}

		n := min(len(buf), f.buffered)
		first := copy(buf[:n], f.input[f.head:])
		copy(buf[first:n], f.input[:n-first])
		f.head = (f.head + n) % len(f.input)
		f.buffered -= n
		f.mu.Unlock()

		written, err := f.conn.Write(buf[:n])
		if written > 0 {
			f.mu.Lock()
			f.pending -= written
			f.mu.Unlock()

			var ack [4]byte
			binary.BigEndian.PutUint32(ack[:], uint32(written))
			if sendErr := f.send(proto.TCPTypeAck, ack[:]); sendErr != nil {
				return sendErr
			}
		}

		if err != nil {
			return err
		}

		if written != n {
			return io.ErrShortWrite
		}
	}
}
