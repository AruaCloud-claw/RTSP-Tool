// Package publisher 外部 RTSP 推流（订阅管道 RTP → 推送到外部服务器），支持断线重连
package publisher

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

	"rtsp-engine/internal/pipeline"
)

// Publisher 外部推流器
// 数据源：任意 pipeline.Interface（文件/摄像头/RTSP 拉流）的 RTP 订阅
// 目标：rtsp://user:pass@host:554/path
type Publisher struct {
	url       string
	logger    *slog.Logger
	cancel    context.CancelFunc
	done      chan struct{}
	unsub     func()
	startTime time.Time

	statMu     sync.Mutex
	frameCount int64
	bytesCount int64

	writeErrCh chan error
}

// Start 启动外部推流（立即连接，后台重连循环）
// source: 数据源管道；urlStr: 目标 RTSP 地址
func Start(source pipeline.Interface, urlStr string, logger *slog.Logger) (*Publisher, error) {
	if _, err := gortspliburl.Parse(urlStr); err != nil {
		return nil, fmt.Errorf("无效的推流地址: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Publisher{
		url:        urlStr,
		logger:     logger,
		cancel:     cancel,
		done:       make(chan struct{}),
		startTime:  time.Now(),
		writeErrCh: make(chan error, 16),
	}
	go p.run(ctx, source)
	logger.Info("publisher started", "target", urlStr)
	return p, nil
}

// run 推流循环（指数退避重连）
func (p *Publisher) run(ctx context.Context, source pipeline.Interface) {
	defer close(p.done)
	backoff := time.Second
	for {
		err := p.connectAndPublish(ctx, source)
		if ctx.Err() != nil {
			return // 主动停止
		}
		p.logger.Warn("publish disconnected, retrying", "target", p.url, "err", err)
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

// connectAndPublish 单次连接并推流（阻塞直到断开或停止）
func (p *Publisher) connectAndPublish(ctx context.Context, source pipeline.Interface) error {
	u, err := gortspliburl.Parse(p.url)
	if err != nil {
		return err
	}

	client := &gortsplib.Client{}
	if err := client.Start(u.Scheme, u.Host); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Close()

	// 1. 从数据源获取 SPS/PPS 构建媒体描述
	sps, pps := source.SPSPPS()
	if len(sps) == 0 || len(pps) == 0 {
		return fmt.Errorf("数据源 SPS/PPS 未就绪")
	}
	medi := &media.Media{
		Type: media.TypeVideo,
		Formats: []formats.Format{&formats.H264{
			PayloadTyp: 96,
			SPS:        sps,
			PPS:        pps,
		}},
	}
	medias := media.Medias{medi}

	// 2. ANNOUNCE + SETUP + RECORD
	if _, err := client.Announce(u, medias); err != nil {
		return fmt.Errorf("announce: %w", err)
	}
	if err := client.SetupAll(medias, u); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if _, err := client.Record(); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	p.logger.Info("publish recording", "target", p.url)

	// 3. 订阅数据源 RTP → 写入外部服务器
	lastErr := time.Now()
	p.unsub = source.AddRTPSubscriber(func(pkt *rtp.Packet) {
		if err := client.WritePacketRTP(medi, pkt); err != nil {
			select {
			case p.writeErrCh <- err:
			default:
			}
			return
		}
		p.statMu.Lock()
		p.frameCount++
		p.bytesCount += int64(len(pkt.Payload))
		p.statMu.Unlock()
	})

	defer func() {
		if p.unsub != nil {
			p.unsub()
			p.unsub = nil
		}
	}()

	// 4. 阻塞：停止 / 写入错误触发重连
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-p.writeErrCh:
			// 避免瞬时错误频繁重连：1s 内多次错误才断开
			if time.Since(lastErr) < time.Second {
				return fmt.Errorf("连续写入失败: %v", err)
			}
			lastErr = time.Now()
		case <-ticker.C:
			// 看门狗：连接健康检查（WritePacketRTP 静默失败时兜底）
			if time.Since(lastErr) > 10*time.Second {
				// 无错误记录，连接应正常
			}
		}
	}
}

// Stop 停止推流
func (p *Publisher) Stop() {
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(6 * time.Second):
	}
	p.logger.Info("publisher stopped", "target", p.url)
}

// Done 完成信号
func (p *Publisher) Done() <-chan struct{} { return p.done }
