// RTSP 拉流源管道：外部 RTSP → RTP 透传 → 内置服务（级联），支持断线重连
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	gortspliburl "github.com/bluenviron/gortsplib/v3/pkg/url"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v3"
	"github.com/bluenviron/gortsplib/v3/pkg/formats"
	"github.com/bluenviron/gortsplib/v3/pkg/media"
	"github.com/pion/rtp"

	"rtsp-engine/internal/rtsp"
)

// RTSPPullPipeline 从外部 RTSP 服务器拉流，RTP 透传到本地内置服务
// 零转码：收到的 RTP 包直接转发（仅重写 SSRC 防冲突）
type RTSPPullPipeline struct {
	url     string
	rtspSrv *rtsp.Server
	path    string
	logger  *slog.Logger
	cancel  context.CancelFunc
	done    chan struct{}

	stream *gortsplib.ServerStream
	medi   *media.Media
	localSSRC uint32

	statMu     sync.Mutex
	frameCount int64
	bytesCount int64
	startTime  time.Time

	subMu   sync.RWMutex
	rtpSubs []func(pkt *rtp.Packet)

	lastPktMu sync.Mutex
	lastPktAt time.Time
}

// StartRTSPPull 启动 RTSP 拉流管道（立即开始连接，后台重连循环）
func StartRTSPPull(urlStr string, rtspSrv *rtsp.Server, path string, logger *slog.Logger) (*RTSPPullPipeline, error) {
	if _, err := gortspliburl.Parse(urlStr); err != nil {
		return nil, fmt.Errorf("无效的 RTSP 地址: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &RTSPPullPipeline{
		url:       urlStr,
		rtspSrv:   rtspSrv,
		path:      path,
		logger:    logger,
		cancel:    cancel,
		done:      make(chan struct{}),
		startTime: time.Now(),
		localSSRC: 0x52545053, // "RTPS" 固定本地 SSRC
	}
	go p.run(ctx)
	return p, nil
}

// run 连接循环（指数退避重连）
func (p *RTSPPullPipeline) run(ctx context.Context) {
	defer close(p.done)
	backoff := time.Second
	for {
		err := p.connectAndRead(ctx)
		if ctx.Err() != nil {
			return // 主动停止
		}
		p.logger.Warn("rtsp pull disconnected", "url", p.url, "err", err)
		// 断开时注销路径（下次连接重新发布）
		p.rtspSrv.Unpublish(p.path)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// connectAndRead 单次连接并读取（阻塞直到断开或停止）
func (p *RTSPPullPipeline) connectAndRead(ctx context.Context) error {
	u, err := gortspliburl.Parse(p.url)
	if err != nil {
		return err
	}

	// 1. 连接 + DESCRIBE
	client := &gortsplib.Client{}
	if err := client.Start(u.Scheme, u.Host); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Close()
	p.logger.Info("rtsp pull connected", "url", p.url)

	medias, _, _, err := client.Describe(u)
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}

	// 2. 提取 H.264 格式（SPS/PPS）
	var h264f *formats.H264
	for _, m := range medias {
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
		return fmt.Errorf("源不是 H.264 视频流（暂仅支持 H.264）")
	}
	if len(h264f.SPS) == 0 || len(h264f.PPS) == 0 {
		return fmt.Errorf("源 SDP 未提供 SPS/PPS，无法建立本地流")
	}

	// 3. 发布本地路径（重连时复用 media/stream）
	if p.stream == nil {
		p.medi = &media.Media{
			Type: media.TypeVideo,
			Formats: []formats.Format{&formats.H264{
				PayloadTyp: 96,
				SPS:        h264f.SPS,
				PPS:        h264f.PPS,
			}},
		}
	}
	st, err := p.rtspSrv.Publish(p.path, p.medi)
	if err != nil {
		return fmt.Errorf("publish local: %w", err)
	}
	p.stream = st

	// 4. SETUP + PLAY
	if err := client.SetupAll(medias, u); err != nil {
		return fmt.Errorf("setup: %w", err)
	}

	// ⚠️ RTP 回调必须在 Setup 之后注册（OnPacketRTPAny 依赖 setup 后的 c.medias）
	// 在 Setup 前注册会导致回调静默丢失（medias 为空）
	client.OnPacketRTPAny(func(m *media.Media, f formats.Format, pkt *rtp.Packet) {
		if m.Type != media.TypeVideo {
			return
		}
		// 拷贝并重写 SSRC（防多路源冲突）
		cp := *pkt
		cp.SSRC = p.localSSRC
		if err := p.stream.WritePacketRTP(p.medi, &cp); err != nil {
			return
		}
		p.broadcastRTP(&cp)
		p.statMu.Lock()
		p.frameCount++
		p.bytesCount += int64(len(cp.Payload))
		p.statMu.Unlock()
		p.lastPktMu.Lock()
		p.lastPktAt = time.Now()
		p.lastPktMu.Unlock()
	})

	if _, err := client.Play(nil); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	p.logger.Info("rtsp pull playing", "url", p.url, "path", p.path)

	// 6. 阻塞直到断开（watchdog：8s 无 RTP 判定断线）
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			p.lastPktMu.Lock()
			idle := time.Since(p.lastPktAt)
			p.lastPktMu.Unlock()
			if p.lastPktAt.IsZero() {
				continue // 还没收到首个包
			}
			if idle > 8*time.Second {
				return fmt.Errorf("8s 无 RTP 数据（上游断开）")
			}
		}
	}
}

// Stop 停止拉流
func (p *RTSPPullPipeline) Stop() {
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(6 * time.Second):
	}
	p.rtspSrv.Unpublish(p.path)
	p.logger.Info("rtsp pull stopped", "path", p.path)
}

// AddRTPSubscriber 注册 RTP 订阅者（WebRTC 预览 / 外部推流）
func (p *RTSPPullPipeline) AddRTPSubscriber(fn func(pkt *rtp.Packet)) func() {
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

// SPSPPS 返回 SPS/PPS
func (p *RTSPPullPipeline) SPSPPS() ([]byte, []byte) {
	if p.medi == nil || len(p.medi.Formats) == 0 {
		return nil, nil
	}
	if hf, ok := p.medi.Formats[0].(*formats.H264); ok {
		return hf.SPS, hf.PPS
	}
	return nil, nil
}

// Stats 统计
func (p *RTSPPullPipeline) Stats() (fps float64, bitrateKbps int64, clients int) {
	p.statMu.Lock()
	elapsed := time.Since(p.startTime).Seconds()
	frames := p.frameCount
	bytes := p.bytesCount
	p.statMu.Unlock()
	if elapsed > 0 {
		fps = float64(frames) / elapsed
		bitrateKbps = int64(float64(bytes) * 8 / elapsed / 1000)
	}
	clients = p.rtspSrv.ClientCount(p.path)
	return
}

// Done 完成信号
func (p *RTSPPullPipeline) Done() <-chan struct{} { return p.done }

func (p *RTSPPullPipeline) broadcastRTP(pkt *rtp.Packet) {
	p.subMu.RLock()
	defer p.subMu.RUnlock()
	if len(p.rtpSubs) == 0 {
		return
	}
	for _, fn := range p.rtpSubs {
		fn(pkt)
	}
}
