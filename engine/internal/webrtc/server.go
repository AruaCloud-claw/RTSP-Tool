// Package webrtc WebRTC 低延迟预览（基于 pion/webrtc）
// 信令：一次性 offer/answer 交换（本机直连，无需 STUN/TURN）
// 媒体：H.264 RTP 直通（与 RTSP 推流共用同一份 RTP 包）
package webrtc

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Manager WebRTC 会话管理器
type Manager struct {
	logger   *slog.Logger
	mu       sync.Mutex
	nextID   int
	sessions map[string]*Session
}

// Session 一个 WebRTC 预览会话
type Session struct {
	id      string
	pc      *webrtc.PeerConnection
	track   *webrtc.TrackLocalStaticRTP
	cancel  func() // 取消 RTP 订阅
	mu      sync.Mutex
	closed  bool
}

// NewManager 创建管理器
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{
		logger:   logger,
		sessions: make(map[string]*Session),
	}
}

// HandleOffer 处理客户端 WebRTC offer，返回 answer SDP
// sps/pps: 流的参数集（用于 SDP sprop-parameter-sets）
// rtpSource: 建立 RTP 订阅的函数（返回取消函数），由调用方绑定到 pipeline
// 返回值：answer SDP 和取消函数
func (m *Manager) HandleOffer(sps, pps []byte, offerSDP string, rtpSource func(func(pkt *rtp.Packet)) func()) (string, func(), error) {
	m.mu.Lock()
	m.nextID++
	sid := fmt.Sprintf("w%d", m.nextID)
	m.mu.Unlock()

	// 1. 创建 PeerConnection
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", nil, fmt.Errorf("new pc: %w", err)
	}

	// 2. 创建 H.264 track（SDP 携带 sprop-parameter-sets，浏览器直接初始化解码器）
	codec := webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: fmt.Sprintf("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=%s;sprop-parameter-sets=%s,%s",
			profileLevelID(sps), b64(sps), b64(pps)),
	}
	track, err := webrtc.NewTrackLocalStaticRTP(codec, "video", "video")
	if err != nil {
		pc.Close()
		return "", nil, fmt.Errorf("new track: %w", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		pc.Close()
		return "", nil, fmt.Errorf("add track: %w", err)
	}

	sess := &Session{id: sid, pc: pc, track: track}
	m.mu.Lock()
	m.sessions[sid] = sess
	m.mu.Unlock()

	// 3. 设置远端 offer
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		sess.close()
		m.remove(sid)
		return "", nil, fmt.Errorf("set remote description: %w", err)
	}

	// 4. 创建 answer，等待 ICE 候选收集完成（非 trickle）
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		sess.close()
		m.remove(sid)
		return "", nil, fmt.Errorf("create answer: %w", err)
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		sess.close()
		m.remove(sid)
		return "", nil, fmt.Errorf("set local description: %w", err)
	}
	<-gatherDone

	// 5. 绑定 RTP 源
	var pktLogCount int
	cancel := rtpSource(func(pkt *rtp.Packet) {
		if len(pkt.Payload) > 0 {
			t := pkt.Payload[0] & 0x1f
			if t == 7 || t == 8 {
				m.logger.Info("webrtc got spspps", "session", sid, "seq", pkt.SequenceNumber, "ts", pkt.Timestamp, "len", len(pkt.Payload))
			}
			if pktLogCount < 60 {
				pktLogCount++
				m.logger.Info("webrtc pkt", "session", sid, "seq", pkt.SequenceNumber, "nal", t)
			}
		}
		if err := track.WriteRTP(pkt); err != nil {
			// 客户端断开等场景：忽略，由连接状态清理
			m.logger.Warn("webrtc write", "session", sid, "err", err)
		}
	})
	sess.cancel = cancel

	// 6. 连接状态监控
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		m.logger.Debug("webrtc state", "session", sid, "state", state.String())
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			sess.close()
			m.remove(sid)
		}
	})

	// 7. answer SDP 注入 sprop-parameter-sets
	// pion 协商时，answer 的 fmtp 只回显 offer 携带的参数（接收方向 offer 通常不带 sprop），
	// 而浏览器 H.264 解码器需要 sprop 才能初始化 —— 这里手工注入
	answerSDP := pc.LocalDescription().SDP
	answerSDP = injectSprop(answerSDP, b64(sps), b64(pps))

	m.logger.Info("webrtc session created", "session", sid)
	return answerSDP, func() {
		sess.close()
		m.remove(sid)
	}, nil
}

// injectSprop 在 answer SDP 的第一个 a=fmtp 行追加 sprop-parameter-sets（若未包含）
func injectSprop(sdp, spsB64, ppsB64 string) string {
	if spsB64 == "" || ppsB64 == "" {
		return sdp
	}
	lines := strings.Split(sdp, "\r\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "a=fmtp:") {
			if !strings.Contains(l, "sprop-parameter-sets") {
				lines[i] = l + ";sprop-parameter-sets=" + spsB64 + "," + ppsB64
			}
			break
		}
	}
	return strings.Join(lines, "\r\n")
}

// CloseAll 关闭所有会话（引擎退出时调用）
func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// close 关闭会话（幂等）
func (s *Session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	if s.pc != nil {
		s.pc.Close()
	}
}

// profileLevelID 从 SPS 提取 profile-level-id（SPS 第 1-3 字节）
// SPS 格式: [0]=0x67(type7), [1]=profile_idc, [2]=constraints, [3]=level_idc
func profileLevelID(sps []byte) string {
	if len(sps) >= 4 {
		return fmt.Sprintf("%02X%02X%02X", sps[1], sps[2], sps[3])
	}
	return "42001F"
}

func b64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
