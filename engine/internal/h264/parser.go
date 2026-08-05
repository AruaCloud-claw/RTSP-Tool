// Package h264 Annex-B H.264 流解析（帧切分 / SPS/PPS 提取）
package h264

import (
	"time"
)

// NAL 单元类型（低 5 位）
const (
	NALNonIDR = 1 // 非关键帧 slice
	NALIDR    = 5 // 关键帧 slice
	NALSPS    = 7 // 序列参数集
	NALPPS    = 8 // 图像参数集
)

// Frame 一个视频帧（access unit）
type Frame struct {
	NALUs [][]byte      // 帧内所有 NAL（含前置 SPS/PPS）
	PTS   time.Duration // 时间戳
	Key   bool          // 是否关键帧
}

// Parser 增量式 Annex-B 流解析器
// 将 FFmpeg 输出的裸 H.264 字节流切分为帧，并提取 SPS/PPS
type Parser struct {
	buf     []byte // 待解析缓冲
	started bool   // 是否已看到首个起始码
	fps     float64
	pts     time.Duration
	frame   *Frame     // 当前正在组装的帧
	sps     []byte     // 最新 SPS（用于 SDP）
	pps     []byte     // 最新 PPS（用于 SDP）
	frameNo uint64
}

// maxBufSize 缓冲上限：超过则丢弃最旧数据（防止异常流导致内存膨胀）
// 正常 H.264 单帧远小于此值；此限制只在数据长时间无起始码时触发
const maxBufSize = 4 * 1024 * 1024

// NewParser 创建解析器
// fps: 输入流帧率（用于生成 PTS），0 时默认 25
func NewParser(fps float64) *Parser {
	if fps <= 0 {
		fps = 25
	}
	return &Parser{fps: fps}
}

// SPS 返回最近一次 SPS（无则 nil）
func (p *Parser) SPS() []byte { return p.sps }

// PPS 返回最近一次 PPS（无则 nil）
func (p *Parser) PPS() []byte { return p.pps }

// Write 喂入字节流，返回解析出的完整帧
func (p *Parser) Write(data []byte) []*Frame {
	p.buf = append(p.buf, data...)
	var frames []*Frame
	for {
		// 找下一个起始码
		startIdx := -1
		for i := 0; i+3 < len(p.buf); i++ {
			if p.buf[i] == 0 && p.buf[i+1] == 0 {
				if p.buf[i+2] == 1 { // 00 00 01
					startIdx = i
					break
				}
				if i+3 < len(p.buf) && p.buf[i+2] == 0 && p.buf[i+3] == 1 { // 00 00 00 01
					startIdx = i
					break
				}
			}
		}
		if startIdx < 0 {
			// 未找到完整起始码：
			// ⚠️ 不能丢弃数据！管道小块读取时，块内可能只有上一 NAL 的尾部
			// （起始码在下一块）。丢弃会导致大量丢帧。
			// 保留数据等待下一次 Write 拼接；仅超限时丢弃最旧数据防内存膨胀。
			if len(p.buf) > maxBufSize {
				p.buf = p.buf[len(p.buf)-maxBufSize:]
			}
			break
		}

		// 起始码前的数据是一个完整 NAL
		nalu := p.buf[:startIdx]
		if p.started {
			// 去掉起始码本身（nalu 前 3~4 字节）
			p.emitNALU(nalu, &frames)
		} else {
			p.started = true
		}
		// 确定起始码长度并继续
		prefixLen := 3
		if startIdx+3 < len(p.buf) && p.buf[startIdx+2] == 0 {
			prefixLen = 4
		}
		p.buf = p.buf[startIdx+prefixLen:]
	}
	return frames
}

// emitNALU 处理一个完整 NAL（不含起始码）
func (p *Parser) emitNALU(nalu []byte, frames *[]*Frame) {
	if len(nalu) == 0 {
		return
	}
	typ := nalu[0] & 0x1f

	switch typ {
	case NALSPS:
		p.sps = append([]byte(nil), nalu...)
		if p.frame != nil {
			p.frame.NALUs = append(p.frame.NALUs, nalu)
		}
	case NALPPS:
		p.pps = append([]byte(nil), nalu...)
		if p.frame != nil {
			p.frame.NALUs = append(p.frame.NALUs, nalu)
		}
	case NALIDR, NALNonIDR:
		// VCL NAL（slice）：按 first_mb_in_slice 判断帧边界
		// first_mb_in_slice == 0 → 新帧开始；> 0 → 当前帧的后续 slice（多 slice 帧）
		if p.firstMBInSlice(nalu) == 0 {
			// 新帧：先结算上一帧
			if p.frame != nil && len(p.frame.NALUs) > 0 {
				p.frame.PTS = p.nextPTS()
				f := p.frame
				*frames = append(*frames, f)
			}
			// 新帧：仅关键帧（IDR）附加 SPS/PPS（标准流只在关键帧前有 SPS/PPS）
			p.frame = &Frame{Key: typ == NALIDR}
			if typ == NALIDR {
				if p.sps != nil {
					p.frame.NALUs = append(p.frame.NALUs, p.sps)
				}
				if p.pps != nil {
					p.frame.NALUs = append(p.frame.NALUs, p.pps)
				}
			}
		} else if p.frame == nil {
			// 非首片但没有进行中的帧（异常流）：丢弃，避免脏数据
			return
		}
		p.frame.NALUs = append(p.frame.NALUs, nalu)
	default:
		// 其他 NAL（SEI/AUD 等）：附加到当前帧
		if p.frame != nil {
			p.frame.NALUs = append(p.frame.NALUs, nalu)
		}
	}
}

// firstMBInSlice 解析 slice 首片标记（first_mb_in_slice，ue(v)），解析失败返回 0（视为新帧）
func (p *Parser) firstMBInSlice(nalu []byte) uint32 {
	if len(nalu) < 2 {
		return 0
	}
	bitPos := 8 // 跳过 NAL header（1 字节）
	v, ok := readUE(nalu, &bitPos)
	if !ok {
		return 0
	}
	return v
}

// readUE 读取 ue(v)（无符号 Exp-Golomb）
func readUE(data []byte, bitPos *int) (uint32, bool) {
	zeroCount := 0
	for *bitPos < len(data)*8 {
		bit := (data[*bitPos/8] >> (7 - *bitPos%8)) & 1
		*bitPos++
		if bit == 1 {
			break
		}
		zeroCount++
		if zeroCount > 31 {
			return 0, false
		}
	}
	if zeroCount > 31 || *bitPos+zeroCount > len(data)*8 {
		return 0, false
	}
	val := uint32(1)
	for i := 0; i < zeroCount; i++ {
		bit := (data[*bitPos/8] >> (7 - *bitPos%8)) & 1
		*bitPos++
		val = (val << 1) | uint32(bit)
	}
	return val - 1, true
}

// Finish 流结束，返回剩余帧
func (p *Parser) Finish() []*Frame {
	var frames []*Frame
	if p.frame != nil && len(p.frame.NALUs) > 0 {
		p.frame.PTS = p.nextPTS()
		frames = append(frames, p.frame)
		p.frame = nil
	}
	return frames
}

func (p *Parser) nextPTS() time.Duration {
	pts := p.pts
	p.frameNo++
	p.pts = time.Duration(float64(p.frameNo) / p.fps * float64(time.Second))
	return pts
}

// NALType 返回 NAL 类型（低 5 位）
func NALType(b byte) byte { return b & 0x1f }
