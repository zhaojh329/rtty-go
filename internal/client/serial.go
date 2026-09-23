/* SPDX-License-Identifier: MIT */

package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog/log"
	"github.com/zhaojh329/rtty-go/proto"
	"go.bug.st/serial"
)

type SerialSession struct {
	cli     *RttyClient
	sid     string
	port    serial.Port
	input   chan []byte
	done    chan struct{}
	closed  atomic.Bool
	ackMu   sync.Mutex
	ackCond *sync.Cond
	unacked int
}

func handleSerialPortsMsg(cli *RttyClient, data []byte) error {
	id := data[:32]
	ports, err := serial.GetPortsList()
	if err != nil {
		log.Error().Err(err).Msg("failed to list serial ports")
		return cli.WriteMsg(proto.MsgTypeSerialPorts, id, proto.SerialFailed)
	}

	slices.Sort(ports)
	ports = slices.Compact(ports)
	payload, err := json.Marshal(ports)
	if err != nil || len(payload) > 65000 {
		return cli.WriteMsg(proto.MsgTypeSerialPorts, id, proto.SerialFailed)
	}

	return cli.WriteMsg(proto.MsgTypeSerialPorts, id, proto.SerialOK, payload)
}

func handleSerialOpenMsg(cli *RttyClient, data []byte) error {
	sid := string(data[:32])
	settings, err := proto.ParseSerialSettings(data[32:])
	if err != nil {
		return cli.WriteMsg(proto.MsgTypeSerialOpen, sid, proto.SerialInvalidSettings)
	}

	ports, err := serial.GetPortsList()
	if err != nil {
		log.Error().Err(err).Msg("failed to list serial ports before opening")
		return cli.WriteMsg(proto.MsgTypeSerialOpen, sid, proto.SerialFailed)
	}

	if !slices.Contains(ports, settings.Port) {
		return cli.WriteMsg(proto.MsgTypeSerialOpen, sid, proto.SerialNotFound)
	}

	cli.mu.Lock()
	if cli.ntty+cli.nserial >= rttyTermLimit {
		cli.mu.Unlock()
		return cli.WriteMsg(proto.MsgTypeSerialOpen, sid, proto.SerialBusy)
	}

	cli.nserial++
	cli.mu.Unlock()

	mode := &serial.Mode{BaudRate: settings.BaudRate, DataBits: settings.DataBits}
	if settings.StopBits == 2 {
		mode.StopBits = serial.TwoStopBits
	}
	switch settings.Parity {
	case proto.SerialParityOdd:
		mode.Parity = serial.OddParity
	case proto.SerialParityEven:
		mode.Parity = serial.EvenParity
	}

	port, err := serial.Open(settings.Port, mode)
	if err != nil {
		cli.mu.Lock()
		cli.nserial--
		cli.mu.Unlock()

		code := proto.SerialFailed
		if portErr, ok := errors.AsType[*serial.PortError](err); ok {
			switch portErr.Code() {
			case serial.PortBusy:
				code = proto.SerialBusy
			case serial.PortNotFound:
				code = proto.SerialNotFound
			case serial.PermissionDenied:
				code = proto.SerialPermission
			case serial.InvalidSpeed, serial.InvalidDataBits, serial.InvalidStopBits, serial.InvalidParity:
				code = proto.SerialInvalidSettings
			}
		}

		log.Error().Err(err).Msgf("failed to open serial port %s", settings.Port)
		return cli.WriteMsg(proto.MsgTypeSerialOpen, sid, code)
	}

	s := &SerialSession{
		cli: cli, sid: sid, port: port,
		input: make(chan []byte, 16), done: make(chan struct{}),
	}
	s.ackCond = sync.NewCond(&s.ackMu)
	cli.sessions.Store(sid, s)

	if err := cli.WriteMsg(proto.MsgTypeSerialOpen, sid, proto.SerialOK); err != nil {
		s.stop()
		return err
	}

	go s.writeLoop()
	go s.run()
	return nil
}

func (s *SerialSession) run() {
	buf := make([]byte, 4096)
	for {
		n, err := s.port.Read(buf)
		if n > 0 {
			s.ackMu.Lock()
			s.unacked += n
			s.ackMu.Unlock()

			if writeErr := s.cli.WriteMsg(proto.MsgTypeTermData, s.sid, buf[:n]); writeErr != nil {
				break
			}

			s.ackMu.Lock()
			for s.unacked > 4096 && !s.closed.Load() {
				s.ackCond.Wait()
			}
			s.ackMu.Unlock()
		}

		if err != nil {
			if !s.closed.Load() {
				log.Error().Err(err).Msgf("serial read failed for %s", s.sid)
			}
			break
		}
	}

	s.close()
}

func (s *SerialSession) writeInput(data []byte) {
	select {
	case <-s.done:
		return
	default:
	}

	select {
	case s.input <- bytes.Clone(data):
	case <-s.done:
	default:
		log.Error().Msgf("serial input queue full for %s", s.sid)
		s.close()
	}
}

func (s *SerialSession) writeLoop() {
	for {
		select {
		case data := <-s.input:
			if s.closed.Load() {
				return
			}
			s.writePort(data)
		case <-s.done:
			return
		}
	}
}

func (s *SerialSession) writePort(data []byte) {
	for len(data) > 0 {
		if s.closed.Load() {
			return
		}

		n, err := s.port.Write(data)
		if err == nil && n == 0 {
			err = io.ErrShortWrite
		}
		if err != nil {
			if !s.closed.Load() {
				log.Error().Err(err).Msgf("serial write failed for %s", s.sid)
				s.close()
			}
			return
		}
		data = data[n:]
	}
}

func (s *SerialSession) ack(n uint16) {
	s.ackMu.Lock()
	s.unacked = max(0, s.unacked-int(n))
	s.ackCond.Broadcast()
	s.ackMu.Unlock()
}

func (s *SerialSession) stop() bool {
	if !s.cli.sessions.CompareAndDelete(s.sid, s) {
		return false
	}

	s.closed.Store(true)
	close(s.done)
	s.port.Close()

	s.ackMu.Lock()
	s.ackCond.Broadcast()
	s.ackMu.Unlock()

	s.cli.mu.Lock()
	s.cli.nserial--
	s.cli.mu.Unlock()
	return true
}

func (s *SerialSession) close() {
	if s.stop() {
		if err := s.cli.WriteMsg(proto.MsgTypeLogout, s.sid); err != nil {
			log.Debug().Err(err).Msgf("failed to report closed serial session %s", s.sid)
		}
	}
}
