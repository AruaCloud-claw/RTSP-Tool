// Package pipeline 媒体管道：输入源 → H.264 解析 → RTP 打包 → RTSP 分发
package pipeline

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v3"
	"github.com/bluenviron/gortsplib/v3/pkg/formats"
	"github.com/bluenviron/gortsplib/v3/pkg/formats/rtph264"
	"github.com/bluenviron/gortsplib/v3/pkg/media"
	"github.com/pion/rtp"

	"rtsp-engine/internal/h264"
	"rtsp-engine/internal/rtsp"
	"rtsp-engine/internal/source"
)

// FilePipeline 文件源透传管道
type FilePipeline struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	parser   *h264.Parser
	enc      *rtph264.Encoder
	stream   *gortsplib.ServerStream
	medi     *media.Media
	cancel   context.CancelFunc
	done     chan struct{}
	logger   *slog.Logger
	rtspSrv  *rtsp.Server
	path     string

	statMu     sync.Mutex
	frameCount int64
	bytesCount int64
	startTime  time.Time
	fps        float64

	// RTP 订阅者（WebRTC 预览等），收到每帧的 RTP 包
	subMu        sync.RWMutex
	rtpSubs      []func(pkt *rtp.Packet)
	sps, pps     []byte
}

// StartFile 启动文件源管道（阻塞直到拿到 SPS/PPS 并发布路径，或失败）
func StartFile(ffmpegPath string, args source.FileSourceArgs,
	rtspSrv *rtsp.Server, path string, logger *slog.Logger) (*FilePipeline, error) {

	// 1. 探测文件
	info, err := source.Probe(ffmpegPath, args.FilePath)
	if err != nil {
		return nil, err
	}
	if info.Codec != "h264" {
		return nil, fmt.Errorf("文件编码 %s 暂不支持透传（P1 仅支持 h264，转码模式 P4 支持）", info.Codec)
	}
	fps := args.FPS
	if fps <= 0 {
		fps = info.FPS
	}
	logger.Info("file probed", "file", args.FilePath, "codec", info.Codec,
		"resolution", fmt.Sprintf("%dx%d", info.Width, info.Height), "fps", fps)

	// 2. 构建 FFmpeg 命令
	cmdArgs, err := source.BuildFileCommand(args)
	if err != nil {
		return nil, err
	}
	return startFFmpegPipeline(ffmpegPath, cmdArgs, fps, rtspSrv, path, logger)
}

// StartCamera 启动摄像头源管道（FFmpeg 转码为 H.264）
func StartCamera(ffmpegPath string, args source.CameraSourceArgs,
	rtspSrv *rtsp.Server, path string, logger *slog.Logger) (*FilePipeline, error) {
	cmdArgs, err := source.BuildCameraCommand(args)
	if err != nil {
		return nil, err
	}
	fps := float64(args.Framerate)
	if fps <= 0 {
		fps = 25
	}
	logger.Info("camera starting", "device", source.SanitizeDshowDevice(args.Device),
		"size", fmt.Sprintf("%dx%d", args.Width, args.Height), "fps", fps)
	return startFFmpegPipeline(ffmpegPath, cmdArgs, fps, rtspSrv, path, logger)
}

// stderrTail 保存 ffmpeg stderr 最近 N 行（启动失败时随错误信息返回，定位真实原因）
type stderrTail struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (t *stderrTail) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// startFFmpegPipeline 通用 FFmpeg 源管道：启动进程 → 预读 SPS/PPS → 发布 → 转发 RTP
func startFFmpegPipeline(ffmpegPath string, cmdArgs []string, fps float64,
	rtspSrv *rtsp.Server, path string, logger *slog.Logger) (*FilePipeline, error) {

	// 1. 启动 FFmpeg 进程
	cmd := exec.Command(ffmpegPath, cmdArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// FFmpeg stderr 日志
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	logger.Info("ffmpeg started", "pid", cmd.Process.Pid)

	// 2. 提前排空 stderr（防管道阻塞），并缓存最近行用于错误上报
	tail := &stderrTail{max: 20}
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			tail.add(line)
			logger.Debug("ffmpeg", "path", path, "line", line)
		}
	}()

	// 3. 预读流直到拿到 SPS/PPS（同时保留解析出的帧，避免起播丢帧）；限时 20s 防卡死
	if f, ok := stdout.(*os.File); ok {
		f.SetReadDeadline(time.Now().Add(20 * time.Second))
	}
	parser := h264.NewParser(fps)
	sps, pps, pendingFrames, err := readUntilSPSPPS(parser, stdout, logger)
	if f, ok := stdout.(*os.File); ok {
		f.SetReadDeadline(time.Time{}) // 清除超时，恢复正常读取
	}
	if err != nil {
		cmd.Process.Kill()
		if t := tail.String(); t != "" {
			return nil, fmt.Errorf("读取视频流失败（可能不是 H.264 编码）: %w；ffmpeg 输出:\n%s", err, t)
		}
		return nil, fmt.Errorf("读取视频流失败（可能不是 H.264 编码）: %w", err)
	}

	// 4. 创建 Media + 编码器 + 发布路径
	medi := &media.Media{
		Type: media.TypeVideo,
		Formats: []formats.Format{&formats.H264{
			PayloadTyp: 96,
			SPS:        sps,
			PPS:        pps,
		}},
	}
	enc := &rtph264.Encoder{
		PayloadType:       96,
		PacketizationMode: 1,
	}
	if err := enc.Init(); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("h264 encoder init: %w", err)
	}
	stream, err := rtspSrv.Publish(path, medi)
	if err != nil {
		cmd.Process.Kill()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &FilePipeline{
		cmd:       cmd,
		stdin:     stdin,
		parser:    parser,
		enc:       enc,
		stream:    stream,
		medi:      medi,
		cancel:    cancel,
		done:      make(chan struct{}),
		logger:    logger,
		rtspSrv:   rtspSrv,
		path:      path,
		fps:       fps,
		startTime: time.Now(),
		sps:       sps,
		pps:       pps,
	}

	// 5. 后台读循环
	go p.readLoop(ctx, stdout)

	// 6. 发送预读期间解析出的帧（起播不丢帧）
	for _, f := range pendingFrames {
		p.writeFrame(f)
	}

	// 6. FFmpeg 退出监控
	go func() {
		err := cmd.Wait()
		p.cancel()
		select {
		case <-p.done:
		default:
			close(p.done)
		}
		if err != nil {
			logger.Warn("ffmpeg exited", "path", path, "err", err)
		}
	}()

	return p, nil
}

// readUntilSPSPPS 预读字节流直到解析出 SPS+PPS
// 返回 SPS、PPS 及预读期间已解析的完整帧
func readUntilSPSPPS(p *h264.Parser, rd io.Reader, logger *slog.Logger) ([]byte, []byte, []*h264.Frame, error) {
	br := bufio.NewReaderSize(rd, 64*1024)
	buf := make([]byte, 64*1024)
	var frames []*h264.Frame
	for i := 0; i < 1024; i++ { // 最多读 64MB，防止死等
		n, err := br.Read(buf)
		if n > 0 {
			frames = append(frames, p.Write(buf[:n])...)
			if p.SPS() != nil && p.PPS() != nil {
				return p.SPS(), p.PPS(), frames, nil
			}
		}
		if err != nil {
			return nil, nil, frames, err
		}
	}
	return nil, nil, frames, fmt.Errorf("在 %dMB 内未找到 SPS/PPS", 64)
}

// readLoop 主读循环：解析帧 → RTP 编码 → 写入 ServerStream
func (p *FilePipeline) readLoop(ctx context.Context, rd io.Reader) {
	defer func() {
		p.cancel()
		select {
		case <-p.done:
		default:
			close(p.done)
		}
	}()
	br := bufio.NewReaderSize(rd, 256*1024)
	buf := make([]byte, 256*1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := br.Read(buf)
		if n > 0 {
			for _, frame := range p.parser.Write(buf[:n]) {
				p.writeFrame(frame)
			}
		}
		if err != nil {
			if err != io.EOF {
				p.logger.Warn("read ffmpeg stdout", "path", p.path, "err", err)
			}
			// 收尾剩余帧
			for _, frame := range p.parser.Finish() {
				p.writeFrame(frame)
			}
			return
		}
	}
}

// writeFrame 编码并发送一帧
func (p *FilePipeline) writeFrame(frame *h264.Frame) {
	pkts, err := p.enc.Encode(frame.NALUs, frame.PTS)
	if err != nil {
		p.logger.Warn("h264 encode", "path", p.path, "err", err)
		return
	}
	for _, pkt := range pkts {
		if err := p.stream.WritePacketRTP(p.medi, pkt); err != nil {
			p.logger.Warn("write rtp", "path", p.path, "err", err)
			return
		}
		p.broadcastRTP(pkt)
	}
	p.statMu.Lock()
	p.frameCount++
	p.bytesCount += int64(len(frame.NALUs[0]))
	p.statMu.Unlock()
}

// Stop 优雅停止管道
func (p *FilePipeline) Stop() {
	// 优雅退出：向 ffmpeg stdin 发 'q'
	if p.stdin != nil {
		io.WriteString(p.stdin, "q")
	}
	// 等待退出（上限 5s）
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		p.cancel()
		if p.cmd != nil && p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
	}
	// 注销路径
	p.rtspSrv.Unpublish(p.path)
	p.logger.Info("pipeline stopped", "path", p.path)
}

// Stats 返回实时统计（帧率、码率 kbps、客户端数）
func (p *FilePipeline) Stats() (fps float64, bitrateKbps int64, clients int) {
	p.statMu.Lock()
	defer p.statMu.Unlock()
	elapsed := time.Since(p.startTime).Seconds()
	if elapsed > 0 {
		fps = float64(p.frameCount) / elapsed
		bitrateKbps = int64(float64(p.bytesCount) * 8 / elapsed / 1000)
	}
	clients = p.rtspSrv.ClientCount(p.path)
	return
}

// Done 返回完成信号
func (p *FilePipeline) Done() <-chan struct{} { return p.done }

// AddRTPSubscriber 注册 RTP 包订阅者（WebRTC 预览等），返回取消函数
func (p *FilePipeline) AddRTPSubscriber(fn func(pkt *rtp.Packet)) func() {
	p.subMu.Lock()
	p.rtpSubs = append(p.rtpSubs, fn)
	p.subMu.Unlock()
	return func() {
		p.subMu.Lock()
		defer p.subMu.Unlock()
		for i, f := range p.rtpSubs {
			if &f == &fn {
				p.rtpSubs = append(p.rtpSubs[:i], p.rtpSubs[i+1:]...)
				break
			}
		}
	}
}

// SPSPPS 返回流的 SPS/PPS（WebRTC SDP sprop-parameter-sets 用）
func (p *FilePipeline) SPSPPS() ([]byte, []byte) { return p.sps, p.pps }

// broadcastRTP 向所有订阅者广播 RTP 包
func (p *FilePipeline) broadcastRTP(pkt *rtp.Packet) {
	p.subMu.RLock()
	defer p.subMu.RUnlock()
	if len(p.rtpSubs) == 0 {
		return
	}
	for _, fn := range p.rtpSubs {
		fn(pkt)
	}
}

// BinDir 返回可执行文件所在目录（用于定位 ffprobe）
func BinDir(exePath string) string {
	if exePath == "" {
		return ""
	}
	return filepath.Dir(exePath)
}
