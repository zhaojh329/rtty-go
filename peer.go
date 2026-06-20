/* SPDX-License-Identifier: MIT */
/*
 * Author: Jianhui Zhao <zhaojh329@gmail.com>
 */

package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog/log"
	"github.com/zhaojh329/rtty-go/proto"
)

type peerICEServerConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type peerOfferEnvelope struct {
	Offer      webrtc.SessionDescription `json:"offer"`
	ICEServers []peerICEServerConfig     `json:"iceServers,omitempty"`
}

type peerControlMessage struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

const (
	peerTerminalBufferedAmountLow  = uint64(256 * 1024)
	peerTerminalBufferedAmountHigh = uint64(512 * 1024)
	peerTerminalDrainTimeout       = 5 * time.Second

	peerStatePending = uint32(iota)
	peerStateConnecting
	peerStateReady
	peerStateFallback
)

type PeerSession struct {
	sid            string
	mu             sync.Mutex
	pc             *webrtc.PeerConnection
	terminalDC     *webrtc.DataChannel
	terminalLow    chan struct{}
	state          atomic.Uint32
	lastSignalAt   atomic.Int64
	lastSignalType atomic.Uint32
	createdAt      time.Time
}

type PeerSessionManager struct {
	cli      *RttyClient
	sessions sync.Map
}

func newPeerSessionManager(cli *RttyClient) *PeerSessionManager {
	return &PeerSessionManager{cli: cli}
}

func (m *PeerSessionManager) Handle(data []byte) error {
	sid := string(data[:32])
	signalType := data[32]
	payload := data[33:]

	session := m.getOrCreate(sid)
	session.lastSignalAt.Store(time.Now().Unix())
	session.lastSignalType.Store(uint32(signalType))

	log.Debug().Msgf("recv peer signal: sid=%s type=%d payload=%dB", sid, signalType, len(payload))

	switch signalType {
	case proto.PeerSignalOffer:
		return m.handleOffer(session, payload)
	case proto.PeerSignalCandidate:
		return m.handleCandidate(session, payload)
	default:
		return m.fallback(session, fmt.Sprintf("unsupported peer signal type %d", signalType))
	}
}

func (m *PeerSessionManager) Delete(sid string) {
	if v, ok := m.sessions.LoadAndDelete(sid); ok {
		v.(*PeerSession).Close()
	}
}

func (m *PeerSessionManager) Close() {
	m.sessions.Range(func(key, value any) bool {
		value.(*PeerSession).Close()
		m.sessions.Delete(key)
		return true
	})
}

func (m *PeerSessionManager) getOrCreate(sid string) *PeerSession {
	if v, ok := m.sessions.Load(sid); ok {
		return v.(*PeerSession)
	}

	session := &PeerSession{
		sid:         sid,
		terminalLow: make(chan struct{}, 1),
		createdAt:   time.Now(),
	}
	session.state.Store(peerStatePending)

	if existing, loaded := m.sessions.LoadOrStore(sid, session); loaded {
		return existing.(*PeerSession)
	}

	return session
}

func (m *PeerSessionManager) handleOffer(session *PeerSession, payload []byte) error {
	var envelope peerOfferEnvelope
	var offer webrtc.SessionDescription
	iceServers := []webrtc.ICEServer{}

	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Offer.SDP != "" {
		offer = envelope.Offer
		iceServers = toWebRTCIceServers(envelope.ICEServers)
	} else if err := json.Unmarshal(payload, &offer); err != nil {
		return m.fallback(session, fmt.Sprintf("invalid offer payload: %v", err))
	}

	pc, err := m.newPeerConnection(session, iceServers)
	if err != nil {
		return m.fallback(session, fmt.Sprintf("create peer connection failed: %v", err))
	}

	session.state.Store(peerStateConnecting)

	if err := pc.SetRemoteDescription(offer); err != nil {
		return m.fallback(session, fmt.Sprintf("set remote description failed: %v", err))
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return m.fallback(session, fmt.Sprintf("create answer failed: %v", err))
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		return m.fallback(session, fmt.Sprintf("set local description failed: %v", err))
	}

	answerPayload, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		return m.fallback(session, fmt.Sprintf("marshal answer failed: %v", err))
	}

	return m.cli.SendPeerSignal(session.sid, proto.PeerSignalAnswer, answerPayload)
}

func (m *PeerSessionManager) handleCandidate(session *PeerSession, payload []byte) error {
	pc := session.peerConnection()
	if pc == nil {
		return nil
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal(payload, &candidate); err != nil {
		return m.fallback(session, fmt.Sprintf("invalid candidate payload: %v", err))
	}

	if err := pc.AddICECandidate(candidate); err != nil {
		return m.fallback(session, fmt.Sprintf("add candidate failed: %v", err))
	}

	return nil
}

func (m *PeerSessionManager) newPeerConnection(session *PeerSession, iceServers []webrtc.ICEServer) (*webrtc.PeerConnection, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICETransportPolicy: webrtc.ICETransportPolicyAll, ICEServers: iceServers})
	if err != nil {
		return nil, err
	}

	session.replacePeerConnection(pc)

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		payload, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			log.Error().Err(err).Msgf("failed to marshal ICE candidate for %s", session.sid)
			return
		}

		if err := m.cli.SendPeerSignal(session.sid, proto.PeerSignalCandidate, payload); err != nil {
			log.Error().Err(err).Msgf("failed to send ICE candidate for %s", session.sid)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Debug().Msgf("peer connection state for %s: %s", session.sid, state.String())

		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			if err := m.fallback(session, fmt.Sprintf("peer connection state %s", state.String())); err != nil {
				log.Error().Err(err).Msgf("failed to notify fallback for %s", session.sid)
			}
		case webrtc.PeerConnectionStateClosed:
			m.Delete(session.sid)
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Debug().Msgf("peer data channel for %s: label=%s", session.sid, dc.Label())

		if dc.Label() == "terminal" {
			session.setTerminalDataChannel(dc)
		}

		dc.OnOpen(func() {
			session.state.Store(peerStateReady)

			payload, err := json.Marshal(map[string]string{
				"label": dc.Label(),
				"mode":  "peer",
			})
			if err != nil {
				log.Error().Err(err).Msgf("failed to marshal peer ready payload for %s", session.sid)
				return
			}

			if err := m.cli.SendPeerSignal(session.sid, proto.PeerSignalReady, payload); err != nil {
				log.Error().Err(err).Msgf("failed to send peer ready for %s", session.sid)
			}
		})

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			log.Debug().Msgf("peer data channel message for %s: label=%s %dB", session.sid, dc.Label(), len(msg.Data))

			if dc.Label() == "terminal" {
				if msg.IsString {
					m.handleTerminalControl(session.sid, msg.Data)
				} else {
					m.handleTerminalInput(session.sid, msg.Data)
				}
			}
		})
	})

	return pc, nil
}

func (m *PeerSessionManager) fallback(session *PeerSession, reason string) error {
	if session.state.Load() == peerStateFallback {
		return nil
	}

	session.state.Store(peerStateFallback)
	session.Close()

	payload, err := json.Marshal(map[string]string{
		"mode":   "server-relay",
		"reason": reason,
	})
	if err != nil {
		return err
	}

	return m.cli.SendPeerSignal(session.sid, proto.PeerSignalFallbackRelay, payload)
}

func (m *PeerSessionManager) SendTerminalData(sid string, data []byte) (bool, error) {
	v, ok := m.sessions.Load(sid)
	if !ok {
		return false, nil
	}

	session := v.(*PeerSession)
	if session.state.Load() != peerStateReady {
		return false, nil
	}

	dc := session.terminalDataChannel()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return false, nil
	}

	if err := session.waitForTerminalCapacity(dc); err != nil {
		if fbErr := m.fallback(session, fmt.Sprintf("terminal peer backpressure timeout: %v", err)); fbErr != nil {
			log.Error().Err(fbErr).Msgf("failed to send peer fallback for %s", sid)
		}
		return false, nil
	}

	if err := dc.Send(data); err != nil {
		if fbErr := m.fallback(session, fmt.Sprintf("send terminal data failed: %v", err)); fbErr != nil {
			log.Error().Err(fbErr).Msgf("failed to send peer fallback for %s", sid)
		}
		return false, nil
	}

	return true, nil
}

func (m *PeerSessionManager) handleTerminalInput(sid string, data []byte) {
	v, ok := m.cli.sessions.Load(sid)
	if !ok {
		log.Warn().Msgf("terminal session %s not found for peer data", sid)
		return
	}

	s := v.(*TermSession)
	if _, err := s.term.Write(data); err != nil {
		log.Error().Err(err).Msgf("failed to write peer terminal data for %s", sid)
		return
	}

	s.active()
}

func (m *PeerSessionManager) handleTerminalControl(sid string, data []byte) {
	v, ok := m.cli.sessions.Load(sid)
	if !ok {
		log.Warn().Msgf("terminal session %s not found for peer control", sid)
		return
	}

	var msg peerControlMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Error().Err(err).Msgf("invalid peer control message for %s", sid)
		return
	}

	s := v.(*TermSession)

	switch msg.Type {
	case "winsize":
		if err := s.term.SetWinSize(msg.Cols, msg.Rows); err != nil {
			log.Error().Err(err).Msgf("failed to set peer terminal size for %s", sid)
			return
		}
		s.active()
	default:
		log.Warn().Msgf("unknown peer control message for %s: %s", sid, msg.Type)
	}
}

func (s *PeerSession) replacePeerConnection(pc *webrtc.PeerConnection) {
	s.mu.Lock()
	oldPC := s.pc
	s.pc = pc
	s.terminalDC = nil
	s.mu.Unlock()

	if oldPC != nil {
		_ = oldPC.Close()
	}
}

func (s *PeerSession) setTerminalDataChannel(dc *webrtc.DataChannel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dc.SetBufferedAmountLowThreshold(peerTerminalBufferedAmountLow)
	dc.OnBufferedAmountLow(func() {
		s.signalTerminalBufferedLow()
	})
	s.terminalDC = dc
}

func (s *PeerSession) terminalDataChannel() *webrtc.DataChannel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalDC
}

func (s *PeerSession) peerConnection() *webrtc.PeerConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pc
}

func (s *PeerSession) Close() {
	s.mu.Lock()
	pc := s.pc
	s.pc = nil
	s.terminalDC = nil
	s.mu.Unlock()

	if pc != nil {
		_ = pc.Close()
	}
}

func (s *PeerSession) signalTerminalBufferedLow() {
	select {
	case s.terminalLow <- struct{}{}:
	default:
	}
}

func (s *PeerSession) waitForTerminalCapacity(dc *webrtc.DataChannel) error {
	if dc.BufferedAmount() <= peerTerminalBufferedAmountHigh {
		return nil
	}

	timer := time.NewTimer(peerTerminalDrainTimeout)
	defer timer.Stop()

	for dc.BufferedAmount() > peerTerminalBufferedAmountHigh {
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			return fmt.Errorf("data channel state is %s", dc.ReadyState().String())
		}

		select {
		case <-s.terminalLow:
		case <-timer.C:
			return fmt.Errorf("buffered amount %d stayed above %d", dc.BufferedAmount(), peerTerminalBufferedAmountHigh)
		}
	}

	return nil
}

func toWebRTCIceServers(cfg []peerICEServerConfig) []webrtc.ICEServer {
	if len(cfg) == 0 {
		return nil
	}

	servers := make([]webrtc.ICEServer, 0, len(cfg))
	for _, server := range cfg {
		servers = append(servers, webrtc.ICEServer{
			URLs:       append([]string(nil), server.URLs...),
			Username:   server.Username,
			Credential: server.Credential,
		})
	}

	return servers
}
