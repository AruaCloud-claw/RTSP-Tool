// rtsp-engine 主程序：RTSP 推拉流媒体引擎（Go 核心）
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bluenviron/gortsplib/v3/pkg/formats/rtph264"

	pionrtp "github.com/pion/rtp"

	"rtsp-engine/internal/api"
	"rtsp-engine/internal/config"
	"rtsp-engine/internal/ffmpeg"
	"rtsp-engine/internal/pipeline"
	"rtsp-engine/internal/publisher"
	"rtsp-engine/internal/rtsp"
	"rtsp-engine/internal/source"
	"rtsp-engine/internal/stream"
	"rtsp-engine/internal/webrtc"
)

var version = "0.4.2"

func main() {
	cfgPath := flag.String("config", "", "配置文件路径（YAML）")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config failed", "err", err)
		os.Exit(1)
	}

	// 日志
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(cfg.LogLevel),
	}))
	slog.SetDefault(logger)

	// 配置校验：监听地址必须可解析，非法则回退默认值
	// （防止如 "127.0.0.1::8181" 这类错误配置导致端口顺延级联异常）
	if _, _, err := net.SplitHostPort(cfg.HTTPListen); err != nil {
		logger.Warn("invalid http_listen, reset to default", "value", cfg.HTTPListen, "default", config.Default().HTTPListen)
		cfg.HTTPListen = config.Default().HTTPListen
	}
	if _, _, err := net.SplitHostPort(cfg.RTSPListen); err != nil {
		logger.Warn("invalid rtsp_listen, reset to default", "value", cfg.RTSPListen, "default", config.Default().RTSPListen)
		cfg.RTSPListen = config.Default().RTSPListen
	}

	// FFmpeg 路径动态解析：不依赖持久化的绝对路径（软件目录移动/配置陈旧时自动跟随）
	// 优先级：配置路径且存在 → 引擎同目录（绿色版 ffmpeg.exe 与引擎随行）→ PATH
	cfg.FFmpegPath = resolveFFmpegPath(cfg.FFmpegPath, logger)

	logger.Info("rtsp-engine starting",
		"version", version,
		"http", cfg.HTTPListen,
		"rtsp", cfg.RTSPListen,
	)

	// ---------- 端口自动顺延（开箱即用：端口被占时自动用下一个空闲端口） ----------
	// 注意：保持原 host 形式（如 ":8554" 顺延为 ":8555"），快照 URL 拼接与 UI 解析依赖该形式
	httpHost, httpPortStr, _ := net.SplitHostPort(cfg.HTTPListen)
	if httpHost == "" {
		httpHost = "127.0.0.1" // API 服务默认仅本机可访问
	}
	httpStart, _ := strconv.Atoi(httpPortStr)
	if httpStart == 0 {
		httpStart = 18080 // 默认避开 Windows HTTP.sys/WinNAT 常保留的 808x 段
	}

	rtspHost, rtspPortStr, _ := net.SplitHostPort(cfg.RTSPListen)
	rtspStart, _ := strconv.Atoi(rtspPortStr)
	if rtspStart == 0 {
		rtspStart = 8554
	}
	actualRTSP := probeFreeAddr(rtspHost, rtspStart, 100, logger)
	if actualRTSP != cfg.RTSPListen {
		logger.Warn("rtsp port busy, shifted", "configured", cfg.RTSPListen, "actual", actualRTSP)
	}

	// ---------- 组件 ----------
	mgr := stream.NewManager()
	udpRTP, udpRTCP := probeUDPAddrs(actualRTSP, cfg.UDPRTPAddress, cfg.UDPRTCPAddress, logger)
	rtspSrv := rtsp.New(actualRTSP, udpRTP, udpRTCP, cfg.Auth.Username, cfg.Auth.Password, logger)
	if err := rtspSrv.Start(); err != nil {
		logger.Error("rtsp server failed", "err", err)
		os.Exit(1)
	}
	defer rtspSrv.Close()

	// FFmpeg 进程管理（预留：摄像头/转码等场景）
	ff := ffmpeg.NewRunner(cfg.FFmpegPath,
		func(id, line string) { logger.Debug("ffmpeg", "stream", id, "line", line) },
		func(id string, err error) {
			if err != nil {
				logger.Warn("ffmpeg exited with error", "stream", id, "err", err)
			}
		},
	)
	defer ff.StopAll(3 * time.Second)

	// ---------- 推流管道管理 ----------
	var pipeMu sync.Mutex
	pipes := make(map[string]pipeline.Interface)      // streamID → 源管道
	pubs := make(map[string]*publisher.Publisher)     // streamID → 外部推流器

	// startFn: 启动一路流（三种输入源 × 两种输出目标）
	startFn := func(s *stream.Stream) error {
		pipeMu.Lock()
		defer pipeMu.Unlock()

		if _, exists := pipes[s.ID]; exists {
			return nil // 已在运行
		}

		// 目标校验
		path := s.TargetArgs.Path
		if s.TargetType == stream.TargetLocal {
			if path == "" {
				return &configError{msg: "内置服务目标需指定 target_args.path"}
			}
		} else if s.TargetType == stream.TargetRemote {
			if s.TargetArgs.URL == "" {
				return &configError{msg: "外部推流需指定 target_args.url"}
			}
			// 源管道发布到内置服务（供 UI 预览），path 自动生成
			path = "live/" + s.ID
		} else {
			return &configError{msg: "未知目标类型: " + string(s.TargetType)}
		}

		// 1. 启动源管道
		var pl pipeline.Interface
		var err error
		switch s.SourceType {
		case stream.SourceFile:
			if s.SourceArgs.FilePath == "" {
				return &configError{msg: "文件源需指定 file_path"}
			}
			pl, err = pipeline.StartFile(cfg.FFmpegPath, source.FileSourceArgs{
				FilePath: s.SourceArgs.FilePath,
				Loop:     s.SourceArgs.Loop,
				FPS:      float64(s.SourceArgs.Framerate),
			}, rtspSrv, path, logger)
		case stream.SourceCamera:
			if s.SourceArgs.Device == "" {
				return &configError{msg: "摄像头源需指定 device"}
			}
			pl, err = pipeline.StartCamera(cfg.FFmpegPath, source.CameraSourceArgs{
				Device:    s.SourceArgs.Device,
				Width:     s.SourceArgs.Width,
				Height:    s.SourceArgs.Height,
				Framerate: s.SourceArgs.Framerate,
			}, rtspSrv, path, logger)
		case stream.SourceRTSP:
			if s.SourceArgs.URL == "" {
				return &configError{msg: "RTSP 源需指定 url"}
			}
			pl, err = pipeline.StartRTSPPull(s.SourceArgs.URL, rtspSrv, path, logger)
		default:
			return &notSupportedError{msg: "未知输入源类型: " + string(s.SourceType)}
		}
		if err != nil {
			s.SetStatus(stream.StatusError)
			s.LastError = err.Error()
			return err
		}
		pipes[s.ID] = pl

		// 2. 外部推流目标
		if s.TargetType == stream.TargetRemote {
			pub, err := publisher.Start(pl, s.TargetArgs.URL, logger)
			if err != nil {
				pl.Stop()
				delete(pipes, s.ID)
				return err
			}
			pubs[s.ID] = pub
		}

		s.SetStatus(stream.StatusRunning)
		s.StartedAt = time.Now()
		logger.Info("stream started", "id", s.ID, "source", s.SourceType, "target", s.TargetType, "path", path)
		return nil
	}

	// stopFn: 停止一路流
	stopFn := func(s *stream.Stream) error {
		pipeMu.Lock()
		defer pipeMu.Unlock()
		if pub, ok := pubs[s.ID]; ok {
			pub.Stop()
			delete(pubs, s.ID)
		}
		if pl, ok := pipes[s.ID]; ok {
			pl.Stop()
			delete(pipes, s.ID)
		}
		s.SetStatus(stream.StatusStopped)
		logger.Info("stream stopped", "id", s.ID)
		return nil
	}

	// 状态统计刷新（2s 周期）
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			pipeMu.Lock()
			for id, pl := range pipes {
				if s, ok := mgr.Get(id); ok {
					fps, bitrate, clients := pl.Stats()
					s.FPS = fps
					s.Bitrate = bitrate
					s.Clients = clients
				}
			}
			pipeMu.Unlock()
		}
	}()

	// ---------- HTTP API ----------
	apiSrv := api.New(mgr, logger, startFn, stopFn)
	apiSrv.SetVersion(version)

	// WebRTC 预览信令
	wrtc := webrtc.NewManager(logger)
	defer wrtc.CloseAll()
	apiSrv.SetWebRTCHandler(func(streamID, offerSDP string) (string, error) {
		pipeMu.Lock()
		pl, ok := pipes[streamID]
		pipeMu.Unlock()
		if !ok {
			return "", &configError{msg: "流未运行，无法预览"}
		}
		sps, pps := pl.SPSPPS()
		if len(sps) == 0 || len(pps) == 0 {
			return "", &configError{msg: "流参数集未就绪"}
		}
		answer, _, err := wrtc.HandleOffer(sps, pps, offerSDP, func(sub func(pkt *pionrtp.Packet)) func() {
			return pl.AddRTPSubscriber(sub)
		})
		return answer, err
	})

	// 摄像头枚举
	apiSrv.SetCamerasHandler(func() ([]string, error) {
		return source.EnumerateCameras(cfg.FFmpegPath)
	})

	// 外部推流（第三方推送到内置服务）：列表 + WebRTC 预览
	apiSrv.SetExternalHandlers(
		func() []map[string]any {
			var out []map[string]any
			for _, p := range rtspSrv.ExternalPaths() {
				out = append(out, map[string]any{
					"path":    p,
					"clients": rtspSrv.ClientCount(p),
					"url":     "rtsp://127.0.0.1" + actualRTSP + "/" + p,
				})
			}
			return out
		},
		func(path, offerSDP string) (string, error) {
			sps, pps, ok := rtspSrv.ExternalSPSPPS(path)
			if !ok {
				return "", &configError{msg: "外部推流路径不存在: " + path}
			}
			logger.Info("external offer", "path", path, "sps", len(sps), "pps", len(pps))
			answer, _, err := wrtc.HandleOffer(sps, pps, offerSDP, func(sub func(pkt *pionrtp.Packet)) func() {
				// 外部流转封装：解包 → 关键帧附加 SPS/PPS → 重新打包（与内部流格式一致，浏览器可解码）
				tx := newExternalTranscoder(sps, pps, sub, logger)
				unsub, err := rtspSrv.SubscribeRTP(path, tx.wrap)
				if err != nil {
					return func() {}
				}
				return unsub
			})
			return answer, err
		},
	)

	// ---------- 持久化 ----------
	apiSrv.SetSnapshotsDir(filepath.Join(cfg.DataDir, "snapshots"))
	snapDir := filepath.Join(cfg.DataDir, "snapshots")
	streamsFile := filepath.Join(cfg.DataDir, "streams.json")
	os.MkdirAll(snapDir, 0o755)
	if err := mgr.Load(streamsFile); err != nil {
		logger.Warn("load streams", "err", err)
	}
	saveStreams := func() {
		if err := mgr.Save(streamsFile); err != nil {
			logger.Warn("save streams", "err", err)
		}
	}
	apiSrv.SetOnChange(saveStreams)

	// ---------- 快照 ----------
	apiSrv.SetSnapshotHandler(func(streamID string) (string, error) {
		st, ok := mgr.Get(streamID)
		if !ok {
			return "", &configError{msg: "流不存在"}
		}
		if st.Status != stream.StatusRunning {
			return "", &configError{msg: "流未运行，无法快照"}
		}
		path := ""
		if st.TargetType == stream.TargetLocal {
			path = st.TargetArgs.Path
		} else {
			path = "live/" + st.ID
		}
		fname := fmt.Sprintf("%s_%s.jpg", st.ID, time.Now().Format("20060102_150405"))
		outFile := filepath.Join(snapDir, fname)
		authPart := ""
		if cfg.Auth.Username != "" {
			authPart = fmt.Sprintf("%s:%s@", cfg.Auth.Username, cfg.Auth.Password)
		}
		rtspURL := fmt.Sprintf("rtsp://%s127.0.0.1%s/%s", authPart, actualRTSP, path)
		cmd := exec.Command(cfg.FFmpegPath, "-y", "-loglevel", "error",
			"-rtsp_transport", "tcp", "-i", rtspURL,
			"-frames:v", "1", "-q:v", "2", outFile)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("抓帧失败: %w", err)
		}
		logger.Info("snapshot taken", "stream", streamID, "file", fname)
		return fname, nil
	})

	// ---------- 配置读写 ----------
	cfgPathForAPI := *cfgPath
	apiSrv.SetConfigHandlers(
		func() (map[string]any, error) {
			return map[string]any{
				"http_listen":  cfg.HTTPListen,
				"rtsp_listen":  cfg.RTSPListen,
				"ffmpeg_path":  cfg.FFmpegPath,
				"log_level":    cfg.LogLevel,
				"auth":         map[string]string{"username": cfg.Auth.Username, "password": cfg.Auth.Password},
			}, nil
		},
		func(newCfg map[string]any) error {
			// 合并写回：仅覆盖传入字段，保留 ffmpeg_path/log_level/data_dir 等自动管理项
			if v, ok := newCfg["http_listen"].(string); ok && v != "" {
				cfg.HTTPListen = v
			}
			if v, ok := newCfg["rtsp_listen"].(string); ok && v != "" {
				cfg.RTSPListen = v
			}
			if v, ok := newCfg["ffmpeg_path"].(string); ok && v != "" {
				cfg.FFmpegPath = v
			}
			if v, ok := newCfg["log_level"].(string); ok && v != "" {
				cfg.LogLevel = v
			}
			if auth, ok := newCfg["auth"].(map[string]any); ok {
				if u, ok := auth["username"].(string); ok {
					cfg.Auth.Username = u
				}
				if p, ok := auth["password"].(string); ok {
					cfg.Auth.Password = p
				}
			}
			data, err := yaml.Marshal(cfg)
			if err != nil {
				return err
			}
			return os.WriteFile(cfgPathForAPI, data, 0o644)
		},
	)

	// HTTP 服务启动 + 自检（防 HTTP.sys/WinNAT 劫持：/health 非引擎响应则自动顺延下一端口）
	actualHTTP, httpSrv, err := startHTTPServerWithCheck(apiSrv.Handler(), httpHost, httpStart, 100, logger)
	if err != nil {
		logger.Error("http server failed", "err", err)
		os.Exit(1)
	}
	// 向 UI 主进程上报实际监听地址（机器可解析行，勿改动格式）
	fmt.Printf("RTSP_ENGINE_INFO http=%s rtsp=%s\n", actualHTTP, actualRTSP)

	// 优雅退出
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		logger.Info("shutting down...")
		// 停止所有管道
		pipeMu.Lock()
		for id, pub := range pubs {
			pub.Stop()
			delete(pubs, id)
		}
		for id, pl := range pipes {
			pl.Stop()
			delete(pipes, id)
		}
		pipeMu.Unlock()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		httpSrv.Shutdown(shutdownCtx)
	}()

	// 等待退出信号（Serve 已在自检启动的 goroutine 中运行）
	<-ctx.Done()
	logger.Info("rtsp-engine stopped")
}

// startHTTPServerWithCheck 启动 HTTP 服务并自检：端口被占/被 HTTP.sys 劫持时自动顺延
// 自检方式：请求 /health，响应必须含引擎自身的 "status":"ok" 标记（HTTP.sys 的 400 页面不含）
func startHTTPServerWithCheck(h http.Handler, host string, start, maxShift int, logger *slog.Logger) (string, *http.Server, error) {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	for shift := 0; shift <= maxShift; shift++ {
		addr := net.JoinHostPort(host, strconv.Itoa(start+shift))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			logger.Warn("listen failed, trying next port", "addr", addr, "err", err)
			continue
		}
		srv := &http.Server{Handler: h}
		serveErr := make(chan error, 1)
		go func() { serveErr <- srv.Serve(ln) }()

		// 自检（重试 3 次，容忍启动时序）
		ok := false
		for i := 0; i < 3; i++ {
			resp, err := client.Get("http://" + addr + "/health")
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if strings.Contains(string(body), `"status":"ok"`) {
					ok = true
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		if ok {
			logger.Info("http server listening (self-check ok)", "addr", addr)
			return addr, srv, nil
		}
		logger.Warn("http self-check failed (port hijacked by HTTP.sys?), trying next", "addr", addr)
		srv.Close()
		select {
		case <-serveErr:
		case <-time.After(time.Second):
		}
	}
	return "", nil, fmt.Errorf("no usable http port in %d..%d", start, start+maxShift)
}

// listenWithFallback 从 start 端口起尝试监听，端口被占则顺延（最多 maxShift 个），返回实际地址与已绑定监听器
func listenWithFallback(host string, start, maxShift int, logger *slog.Logger) (string, net.Listener, error) {
	for shift := 0; shift <= maxShift; shift++ {
		addr := net.JoinHostPort(host, strconv.Itoa(start+shift))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return addr, ln, nil
		}
		logger.Warn("listen failed, trying next port", "addr", addr, "err", err)
	}
	return "", nil, fmt.Errorf("no free port in %d..%d", start, start+maxShift)
}

// probeFreeAddr 探测空闲端口（供 gortsplib 这类内部绑定的组件使用；探测与绑定间存在极小竞态窗口）
func probeFreeAddr(host string, start, maxShift int, logger *slog.Logger) string {
	for shift := 0; shift <= maxShift; shift++ {
		addr := net.JoinHostPort(host, strconv.Itoa(start+shift))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return addr
		}
		logger.Warn("port in use, trying next", "addr", addr, "err", err)
	}
	return net.JoinHostPort(host, strconv.Itoa(start))
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// resolveFFmpegPath 解析 ffmpeg 可执行文件路径（绿色版动态定位，不依赖持久化绝对路径）
// 优先级：1) 配置路径且存在  2) 引擎可执行文件同目录（ffmpeg.exe / ffmpeg）  3) PATH
func resolveFFmpegPath(configured string, logger *slog.Logger) string {
	if configured != "" && configured != "ffmpeg" {
		if fi, err := os.Stat(configured); err == nil && !fi.IsDir() {
			return configured
		}
		logger.Warn("configured ffmpeg_path not found, resolving dynamically", "path", configured)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, name := range []string{"ffmpeg.exe", "ffmpeg"} {
			cand := filepath.Join(dir, name)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				logger.Info("resolved ffmpeg from engine directory", "path", cand)
				return cand
			}
		}
	}
	logger.Warn("ffmpeg not found in config or engine directory, falling back to PATH")
	return "ffmpeg"
}

type notSupportedError struct{ msg string }

func (e *notSupportedError) Error() string { return e.msg }

// probeUDPAddrs 推导 UDP RTP/RTCP 监听地址，并探测空闲端口（被占则顺延 +2，避免多实例冲突）
func probeUDPAddrs(rtspListen, udpRTP, udpRTCP string, logger *slog.Logger) (string, string) {
	if udpRTP != "" && udpRTCP != "" {
		return udpRTP, udpRTCP
	}
	host, portStr, err := net.SplitHostPort(rtspListen)
	if err != nil {
		return "", "" // 无法推导则禁用 UDP
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 554 {
		return "", ""
	}
	base := port - 554
	if base%2 != 0 {
		base++ // gortsplib 要求 RTP 端口为偶数
	}
	for shift := 0; shift < 100; shift += 2 {
		rtpPort := base + shift
		l1, err1 := net.ListenPacket("udp", net.JoinHostPort(host, strconv.Itoa(rtpPort)))
		if err1 != nil {
			continue
		}
		l2, err2 := net.ListenPacket("udp", net.JoinHostPort(host, strconv.Itoa(rtpPort+1)))
		if err2 != nil {
			l1.Close()
			continue
		}
		l1.Close()
		l2.Close()
		if shift > 0 {
			logger.Warn("udp ports busy, shifted", "base", base, "actual", rtpPort)
		}
		return net.JoinHostPort(host, strconv.Itoa(rtpPort)), net.JoinHostPort(host, strconv.Itoa(rtpPort+1))
	}
	return "", ""
}

type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

// externalTranscoder 外部流转封装（WebRTC 预览用）
// 第三方推流（ffmpeg）带内通常不带 SPS/PPS，浏览器直接转发无法解码；
// 该转码器将 RTP 解包为 NAL 单元，在关键帧前附加 SPS/PPS，再用与内部流相同的
// 编码器重新打包 —— 输出格式与已验证可解码的内部流完全一致。
type externalTranscoder struct {
	dec    *rtph264.Decoder
	enc    *rtph264.Encoder
	sps    []byte
	pps    []byte
	write  func(*pionrtp.Packet)
	logger *slog.Logger
	frames int
}

func newExternalTranscoder(sps, pps []byte, write func(*pionrtp.Packet), logger *slog.Logger) *externalTranscoder {
	tx := &externalTranscoder{sps: sps, pps: pps, write: write, logger: logger}
	tx.dec = &rtph264.Decoder{}
	_ = tx.dec.Init()
	tx.enc = &rtph264.Encoder{PayloadType: 96, PacketizationMode: 1}
	_ = tx.enc.Init()
	return tx
}

// wrap 订阅回调：解包 → 重组 → 重新打包转发
func (tx *externalTranscoder) wrap(pkt *pionrtp.Packet) {
	// 用 marker 边界组装完整存取单元（AU）
	nalus, pts, err := tx.dec.DecodeUntilMarker(pkt)
	if err != nil {
		return // 分片未收齐等，等待后续包
	}
	if len(nalus) == 0 {
		return
	}

	// 关键帧（含 IDR slice）前附加 SPS/PPS
	isKey := false
	for _, n := range nalus {
		if len(n) > 0 && n[0]&0x1f == 5 {
			isKey = true
			break
		}
	}
	if isKey && len(tx.sps) > 0 && len(tx.pps) > 0 {
		nalus = append([][]byte{tx.sps, tx.pps}, nalus...)
	}

	// 重新打包（与内部流相同参数）
	pkts, err := tx.enc.Encode(nalus, pts)
	if err != nil {
		return
	}
	for _, p := range pkts {
		tx.write(p)
	}
	tx.frames++
	if tx.frames <= 3 && tx.logger != nil {
		tx.logger.Info("external transcoded", "key", isKey, "nalus", len(nalus), "pkts", len(pkts))
	}
}


