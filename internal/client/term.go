/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package client

import (
	"encoding/binary"
	"io"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/zhaojh329/rtty-go/proto"
)

const rttyTermTimeout = 600 * time.Second

func handleTermWinsizeMsg(cli *RttyClient, data []byte) error {
	sid := string(data[:32])

	val, ok := cli.sessions.Load(sid)
	if !ok {
		log.Error().Msgf("terminal session %s not found", sid)
		return nil
	}
	s, ok := val.(*TermSession)
	if !ok {
		return nil
	}

	col := binary.BigEndian.Uint16(data[32:34])
	row := binary.BigEndian.Uint16(data[34:36])

	err := s.term.SetWinSize(col, row)
	if err != nil {
		log.Error().Err(err).Msgf("failed to set terminal size for %s", sid)
		return err
	}

	log.Debug().Msgf("setting terminal %s size to %dx%d", sid, col, row)

	return nil
}

func handleTermLoginMsg(cli *RttyClient, data []byte) error {

	sid := string(data)

	var retCode byte

	cli.mu.Lock()
	if cli.ntty+cli.nserial >= rttyTermLimit {
		log.Error().Msgf("maximum number of TTYs reached: %d", cli.ntty)
		retCode = 1
	} else {
		term, err := NewTerminal(cli.cfg.Username)
		if err != nil {
			log.Error().Err(err).Msg("failed to create terminal")
			retCode = 1
		} else {
			log.Info().Msgf("new tty: %d/%d %s", cli.ntty, rttyTermLimit, sid)

			s := &TermSession{
				cli:  cli,
				sid:  sid,
				term: term,
			}

			s.fc = &RttyFileContext{ses: s}

			cli.sessions.Store(sid, s)

			cli.ntty++

			go s.Run()
		}
	}
	cli.mu.Unlock()

	cli.WriteMsg(proto.MsgTypeLogin, sid, retCode)

	return nil
}

type TermSession struct {
	cli   *RttyClient
	sid   string
	term  *Terminal
	timer *time.Timer
	mu    sync.Mutex
	fc    *RttyFileContext
}

func (s *TermSession) Write(buf []byte) (int, error) {
	length := len(buf)

	s.active()

	if s.fc.detect(buf) {
		return length, nil
	}

	s.cli.WriteMsg(proto.MsgTypeTermData, s.sid, buf)

	s.term.WaitAck(length)

	return length, nil
}

func (s *TermSession) writeInput(data []byte) {
	s.term.Write(data)
	s.active()
}

func (s *TermSession) ack(n uint16) {
	s.term.Ack(n)
}

func (s *TermSession) Run() {
	s.mu.Lock()
	if val, ok := s.cli.sessions.Load(s.sid); !ok || val != s {
		s.mu.Unlock()
		return
	}
	s.timer = time.AfterFunc(rttyTermTimeout, func() {
		log.Info().Msgf("tty %s inactive over %v, now kill it", s.sid, rttyTermTimeout)
		s.term.Close()
	})
	s.mu.Unlock()

	if _, err := io.Copy(s, s.term); err != nil {
		log.Error().Err(err).Msgf("error while copying terminal data for %s", s.sid)
	}
	s.close()
}

func (s *TermSession) active() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.timer != nil {
		s.timer.Reset(rttyTermTimeout)
	}
}

func (s *TermSession) stop() bool {
	if !s.cli.sessions.CompareAndDelete(s.sid, s) {
		return false
	}

	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()

	s.term.Close()
	s.fc.reset()

	s.cli.mu.Lock()
	s.cli.ntty--
	s.cli.mu.Unlock()

	log.Info().Msgf("delete tty %s", s.sid)
	return true
}

func (s *TermSession) close() {
	if s.stop() {
		s.cli.WriteMsg(proto.MsgTypeLogout, s.sid)
	}
}
