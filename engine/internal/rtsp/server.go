// Package rtsp 内置 RTSP 服务（基于 gortsplib）
package rtsp

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v3"
	"github.com/bluenviron/gortsplib/v3/pkg/base"
	"github.com/bluenviron/gortsplib/v3/pkg/formats"
	"github.com/bluenviron/gortsplib/v3/pkg/media"
	"github.com/pion/rtp"
)

// Server 内置 RTSP 服务
// 职责：管理流路径注册、响应客户端 DESCRIBE/SETUP/PLAY 信令、支持外部推流（ANNOUNCE/RECORD）、
// RTP 订阅（WebRTC 预览等零转码复用）
type Server struct {
	srv      *gortsplib.Server
	logger   *slog.Logger
	authUser string // 空 = 不鉴权
	authPass string
	mu       sync.RWMutex
	streams  map[string]*gortsplib.ServerStream // path → ServerStream
	medias   map[string]*media.Media            // path → Media（SDP 用）
	clients  map[string]int                     // path → 拉流客户端数
	sessPath map[*gortsplib.ServerSession]string // session → path
	announceSess map[*gortsplib.ServerSession]string // 外部推流 session → path

	// RTP 订阅者（WebRTC 预览等）：path → 订阅函数列表
	subs map[string][]func(*rtp.Packet)
}

// New 创建 RTSP 服务（未启动）
// udpRTP/udpRTCP: UDP 传输端口（空字符串表示该服务器不支持 UDP 传输）
func New(addr, udpRTP, udpRTCP, authUser, authPass string, logger *slog.Logger) *Server {
	s := &Server{
		logger:   logger,
		authUser: authUser,
		authPass: authPass,
		streams:  make(map[string]*gortsplib.ServerStream),
		medias:   make(map[string]*media.Media),
		clients:  make(map[string]int),
		sessPath: make(map[*gortsplib.ServerSession]string),
		announceSess: make(map[*gortsplib.ServerSession]string),
	}
	s.srv = &gortsplib.Server{
		RTSPAddress:   addr,
		UDPRTPAddress: udpRTP,
		UDPRTCPAddress: udpRTCP,
		Handler:        s,
	}
	s.subs = make(map[string][]func(*rtp.Packet))
	return s
}

// Start 启动监听
func (s *Server) Start() error {
	if err := s.srv.Start(); err != nil {
		return fmt.Errorf("rtsp server start: %w", err)
	}
	s.logger.Info("rtsp server listening")
	return nil
}

// Close 关闭服务
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 关闭所有已注册流
	for path, st := range s.streams {
		st.Close()
		delete(s.streams, path)
		delete(s.medias, path)
	}
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

// Publish 注册一路流（发布者端调用）
// 返回 ServerStream 供写入 RTP 包
func (s *Server) Publish(path string, medi *media.Media) (*gortsplib.ServerStream, error) {
	path = normalizePath(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.streams[path]; exists {
		return nil, fmt.Errorf("path already published: %s", path)
	}
	st := gortsplib.NewServerStream(media.Medias{medi})
	s.streams[path] = st
	s.medias[path] = medi
	s.logger.Info("path published", "path", path)
	return st, nil
}

// Unpublish 注销一路流
func (s *Server) Unpublish(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.streams[path]; ok {
		st.Close()
		delete(s.streams, path)
		delete(s.medias, path)
		s.logger.Info("path unpublished", "path", path)
	}
}

// ClientCount 返回某路径的拉流客户端数
func (s *Server) ClientCount(path string) int {
	path = normalizePath(path)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clients[path]
}

// ---------- gortsplib ServerHandler 实现 ----------

func (s *Server) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	s.logger.Debug("conn open", "remote", ctx.Conn.NetConn().RemoteAddr().String())
}

func (s *Server) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	s.logger.Debug("conn close", "err", ctx.Error)
}

func (s *Server) OnSessionOpen(ctx *gortsplib.ServerHandlerOnSessionOpenCtx) {
	s.logger.Debug("session open")
}

func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()
	if path, ok := s.sessPath[ctx.Session]; ok {
		if s.clients[path] > 0 {
			s.clients[path]--
		}
		delete(s.sessPath, ctx.Session)
	}
	// 外部推流 session 关闭：注销路径
	if path, ok := s.announceSess[ctx.Session]; ok {
		if st, exists := s.streams[path]; exists {
			st.Close()
			delete(s.streams, path)
			delete(s.medias, path)
		}
		delete(s.announceSess, ctx.Session)
		s.logger.Info("announce session closed, path unpublished", "path", path)
	}
	s.mu.Unlock()
	s.logger.Debug("session close")
}

// OnDescribe 客户端 DESCRIBE：返回流的 SDP
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	path := normalizePath(ctx.Path)
	s.logger.Debug("describe", "path", path)

	// Basic 鉴权检查
	if s.authUser != "" && !s.checkAuth(ctx.Request.Header["Authorization"]) {
		return &base.Response{
			StatusCode: base.StatusUnauthorized,
			Header: base.Header{
				"WWW-Authenticate": base.HeaderValue{`Basic realm="rtsp"`},
			},
		}, nil, nil
	}

	s.mu.RLock()
	st, ok := s.streams[path]
	s.mu.RUnlock()
	if !ok {
		return &base.Response{
			StatusCode: base.StatusNotFound,
		}, nil, nil
	}
	return &base.Response{
		StatusCode: base.StatusOK,
		Header: base.Header{
			"Public": base.HeaderValue{"OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN, GET_PARAMETER"},
		},
	}, st, nil
}

// OnSetup 客户端 SETUP：绑定传输
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	path := normalizePath(ctx.Path)
	s.mu.Lock()
	st, ok := s.streams[path]
	if ok {
		s.clients[path]++
		s.sessPath[ctx.Session] = path
	}
	s.mu.Unlock()
	if !ok {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnPlay 客户端 PLAY
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnGetParameter 保活
func (s *Server) OnGetParameter(ctx *gortsplib.ServerHandlerOnGetParameterCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnPause 暂停
func (s *Server) OnPause(ctx *gortsplib.ServerHandlerOnPauseCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnRecord 外部推流：接收 RTP（在 SETUP 之后，此时 setuppedMedias 已填充）
func (s *Server) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	s.mu.RLock()
	path, ok := s.announceSess[ctx.Session]
	medi := s.medias[path]
	s.mu.RUnlock()
	if !ok {
		return &base.Response{StatusCode: base.StatusBadRequest}, nil
	}

	// ⚠️ OnPacketRTPAny 依赖 setuppedMedias（SETUP 后才填充），必须在 RECORD 阶段绑定
	ctx.Session.OnPacketRTPAny(func(m *media.Media, f formats.Format, pkt *rtp.Packet) {
		if m.Type != media.TypeVideo {
			return
		}
		if st, exists := s.getStream(path); exists {
			st.WritePacketRTP(medi, pkt)
		}
		s.fanoutRTP(path, pkt)
	})

	s.logger.Info("external publish recording", "path", path)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnAnnounce 外部推流：注册路径并绑定 RTP 转发
func (s *Server) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	path := normalizePath(ctx.Path)
	s.logger.Info("external announce", "path", path)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.streams[path]; exists {
		return &base.Response{StatusCode: base.StatusServiceUnavailable}, nil
	}

	// 提取 H.264 格式
	var h264f *formats.H264
	for _, m := range ctx.Medias {
		if m.Type != media.TypeVideo {
			continue
		}
		for _, f := range m.Formats {
			if hf, ok := f.(*formats.H264); ok {
				h264f = hf
			}
		}
	}
	if h264f == nil {
		return &base.Response{StatusCode: base.StatusNotAcceptable}, nil
	}

	medi := &media.Media{
		Type: media.TypeVideo,
		Formats: []formats.Format{&formats.H264{
			PayloadTyp: 96,
			SPS:        h264f.SPS,
			PPS:        h264f.PPS,
		}},
	}
	st := gortsplib.NewServerStream(media.Medias{medi})
	s.streams[path] = st
	s.medias[path] = medi
	s.announceSess[ctx.Session] = path

	return &base.Response{StatusCode: base.StatusOK}, nil
}

// getStream 安全获取路径对应的 ServerStream
func (s *Server) getStream(path string) (*gortsplib.ServerStream, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.streams[path]
	return st, ok
}

// ExternalPaths 返回当前外部推流（ANNOUNCE）的路径列表
func (s *Server) ExternalPaths() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, path := range s.announceSess {
		out = append(out, path)
	}
	return out
}

// ExternalSPSPPS 返回外部推流路径的 SPS/PPS（供 WebRTC SDP 使用）
func (s *Server) ExternalSPSPPS(path string) ([]byte, []byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	medi, ok := s.medias[path]
	if !ok {
		return nil, nil, false
	}
	for _, f := range medi.Formats {
		if hf, ok := f.(*formats.H264); ok {
			return hf.SPS, hf.PPS, true
		}
	}
	return nil, nil, false
}

// SubscribeRTP 订阅某路径的 RTP 包（WebRTC 预览等）。返回取消函数。
func (s *Server) SubscribeRTP(path string, sub func(*rtp.Packet)) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.streams[path]; !ok {
		return nil, fmt.Errorf("path not found: %s", path)
	}
	s.subs[path] = append(s.subs[path], sub)
	i := len(s.subs[path]) - 1
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		list := s.subs[path]
		if i < len(list) {
			s.subs[path] = append(list[:i], list[i+1:]...)
		}
	}, nil
}

// fanoutRTP 将 RTP 包分发给路径订阅者
func (s *Server) fanoutRTP(path string, pkt *rtp.Packet) {
	s.mu.RLock()
	subs := s.subs[path]
	s.mu.RUnlock()
	for _, sub := range subs {
		sub(pkt)
	}
}

// normalizePath 统一路径格式：去掉前导斜杠
func normalizePath(path string) string {
	return strings.TrimPrefix(path, "/")
}

// checkAuth 校验 Basic Authorization 头
func (s *Server) checkAuth(header base.HeaderValue) bool {
	if len(header) == 0 {
		return false
	}
	const prefix = "Basic "
	line := strings.TrimSpace(header[0])
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len(prefix):]))
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return false
	}
	return parts[0] == s.authUser && parts[1] == s.authPass
}
