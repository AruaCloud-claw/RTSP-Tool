// Package api 引擎 HTTP API 服务
package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"rtsp-engine/internal/stream"
)

// Server HTTP API 服务
type Server struct {
	mgr   *stream.Manager
	log   *slog.Logger
	start func(s *stream.Stream) error
	stop  func(s *stream.Stream) error
	// 版本号（由主程序注入，用于 /health 上报）
	version string
	// WebRTC 预览：处理 offer，返回 answer
	webrtcOffer func(streamID, offerSDP string) (string, error)
	// 外部推流列表（第三方推送到内置服务的路径）
	externalList func() []map[string]any
	// 外部推流 WebRTC 预览
	externalOffer func(path, offerSDP string) (string, error)
	// 摄像头枚举
	cameras func() ([]string, error)
	// 快照：返回快照文件名
	snapshot func(streamID string) (string, error)
	// 配置读写
	configGet func() (map[string]any, error)
	configSet func(cfg map[string]any) error
	// 流变更回调（创建/删除后调用，用于持久化）
	onChange func()
	// 快照静态目录
	snapDir string
}

// New 创建 API 服务
// start/stop 由上层注入（P0 阶段可为空实现，P1 接入真实链路）
func New(mgr *stream.Manager, log *slog.Logger,
	start func(s *stream.Stream) error,
	stop func(s *stream.Stream) error) *Server {
	return &Server{mgr: mgr, log: log, start: start, stop: stop}
}

// SetVersion 设置版本号（/health 返回）
func (s *Server) SetVersion(v string) {
	s.version = v
}

// SetWebRTCHandler 注入 WebRTC 预览信令处理器
func (s *Server) SetWebRTCHandler(h func(streamID, offerSDP string) (string, error)) {
	s.webrtcOffer = h
}

// SetExternalHandlers 注入外部推流列表与预览处理器
func (s *Server) SetExternalHandlers(list func() []map[string]any, offer func(path, offerSDP string) (string, error)) {
	s.externalList = list
	s.externalOffer = offer
}

// SetCamerasHandler 注入摄像头枚举函数
func (s *Server) SetCamerasHandler(h func() ([]string, error)) {
	s.cameras = h
}

// SetSnapshotHandler 注入快照函数
func (s *Server) SetSnapshotHandler(h func(streamID string) (string, error)) {
	s.snapshot = h
}

// SetConfigHandlers 注入配置读写函数
func (s *Server) SetConfigHandlers(get func() (map[string]any, error), set func(cfg map[string]any) error) {
	s.configGet = get
	s.configSet = set
}

// SetOnChange 注入流变更回调
func (s *Server) SetOnChange(h func()) {
	s.onChange = h
}

// SetSnapshotsDir 设置快照静态目录（提供 /snapshots/* 访问）
func (s *Server) SetSnapshotsDir(dir string) {
	s.snapDir = dir
}

// Handler 返回 HTTP 路由
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/streams", s.handleListStreams)
	mux.HandleFunc("POST /api/v1/streams", s.handleCreateStream)
	mux.HandleFunc("GET /api/v1/streams/{id}", s.handleGetStream)
	mux.HandleFunc("DELETE /api/v1/streams/{id}", s.handleDeleteStream)
	mux.HandleFunc("POST /api/v1/streams/{id}/start", s.handleStartStream)
	mux.HandleFunc("POST /api/v1/streams/{id}/stop", s.handleStopStream)
	mux.HandleFunc("POST /api/v1/streams/{id}/webrtc/offer", s.handleWebRTCOffer)
	mux.HandleFunc("GET /api/v1/cameras", s.handleCameras)
	mux.HandleFunc("POST /api/v1/streams/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/v1/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/v1/config", s.handleSetConfig)
	mux.HandleFunc("GET /api/v1/external-streams", s.handleExternalStreams)
	mux.HandleFunc("POST /api/v1/webrtc/offer", s.handleExternalWebRTCOffer)
	mux.HandleFunc("/ws", s.handleWS)
	if s.snapDir != "" {
		mux.Handle("GET /snapshots/", http.StripPrefix("/snapshots/", http.FileServer(http.Dir(s.snapDir))))
	}

	return s.withAccessLog(s.withCORS(mux))
}

// withCORS 允许 Electron UI 跨域访问
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder 捕获响应状态码（供访问日志使用）
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Hijack 支持 WebSocket 升级（gorilla/websocket 依赖 http.Hijacker）
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response does not implement http.Hijacker")
	}
	return hj.Hijack()
}

// Flush 支持流式响应（http.Flusher）
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withAccessLog 记录每个 HTTP 请求（方法/路径/状态码/耗时），便于定位连接问题
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"dur_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ver := s.version
	if ver == "" {
		ver = "unknown"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": ver,
		"time":    time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleListStreams(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.List())
}

func (s *Server) handleCreateStream(w http.ResponseWriter, r *http.Request) {
	var st stream.Stream
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	// 基本校验
	if st.SourceType == "" {
		writeError(w, http.StatusBadRequest, "source_type is required")
		return
	}
	created, err := s.mgr.Create(&st)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.onChange != nil {
		s.onChange()
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleGetStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, ok := s.mgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "stream not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleDeleteStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, ok := s.mgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "stream not found: "+id)
		return
	}
	if st.Status == stream.StatusRunning || st.Status == stream.StatusStarting {
		if s.stop != nil {
			s.stop(st)
		}
	}
	if !s.mgr.Delete(id) {
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if s.onChange != nil {
		s.onChange()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStartStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, ok := s.mgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "stream not found: "+id)
		return
	}
	if s.start == nil {
		writeError(w, http.StatusNotImplemented, "start not implemented yet (P1)")
		return
	}
	if err := s.start(st); err != nil {
		st.SetStatus(stream.StatusError)
		st.LastError = err.Error()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleStopStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, ok := s.mgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "stream not found: "+id)
		return
	}
	if s.stop == nil {
		writeError(w, http.StatusNotImplemented, "stop not implemented yet (P1)")
		return
	}
	if err := s.stop(st); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleWebRTCOffer WebRTC 预览信令：接收 offer，返回 answer
func (s *Server) handleWebRTCOffer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.mgr.Get(id); !ok {
		writeError(w, http.StatusNotFound, "stream not found: "+id)
		return
	}
	if s.webrtcOffer == nil {
		writeError(w, http.StatusNotImplemented, "webrtc preview not enabled")
		return
	}
	var req struct {
		SDP string `json:"sdp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SDP == "" {
		writeError(w, http.StatusBadRequest, "invalid offer")
		return
	}
	answer, err := s.webrtcOffer(id, req.SDP)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"sdp": answer})
}

// handleCameras 枚举系统摄像头
func (s *Server) handleCameras(w http.ResponseWriter, r *http.Request) {
	if s.cameras == nil {
		writeJSON(w, http.StatusOK, []string{})
		return
	}
	cams, err := s.cameras()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cams)
}

// handleSnapshot 抓取流快照
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.mgr.Get(id); !ok {
		writeError(w, http.StatusNotFound, "stream not found: "+id)
		return
	}
	if s.snapshot == nil {
		writeError(w, http.StatusNotImplemented, "snapshot not enabled")
		return
	}
	fname, err := s.snapshot(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"file": fname, "url": "/snapshots/" + fname})
}

// handleGetConfig 读取引擎配置
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if s.configGet == nil {
		writeError(w, http.StatusNotImplemented, "config not enabled")
		return
	}
	cfg, err := s.configGet()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleSetConfig 更新引擎配置（需重启生效）
func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	if s.configSet == nil {
		writeError(w, http.StatusNotImplemented, "config not enabled")
		return
	}
	var cfg map[string]any
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.configSet(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "note": "重启后生效"})
}

// handleExternalStreams 外部推流列表
func (s *Server) handleExternalStreams(w http.ResponseWriter, r *http.Request) {
	if s.externalList == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.externalList())
}

// handleExternalWebRTCOffer 外部推流 WebRTC 预览信令
func (s *Server) handleExternalWebRTCOffer(w http.ResponseWriter, r *http.Request) {
	if s.externalOffer == nil {
		writeError(w, http.StatusNotImplemented, "external preview not enabled")
		return
	}
	var req struct {
		Path string `json:"path"`
		SDP  string `json:"sdp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" || req.SDP == "" {
		writeError(w, http.StatusBadRequest, "path and sdp required")
		return
	}
	answer, err := s.externalOffer(req.Path, req.SDP)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"sdp": answer})
}

// handleWS WebSocket 实时状态推送
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true }, // 本机应用，允许所有来源
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("ws upgrade failed", "err", err)
		return
	}
	defer conn.Close()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			msg := map[string]any{
				"type":    "stats",
				"streams": s.mgr.List(),
				"time":    time.Now().Format(time.RFC3339),
			}
			if s.externalList != nil {
				msg["external"] = s.externalList()
			}
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": strings.TrimSpace(msg)})
}
