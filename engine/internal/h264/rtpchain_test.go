package h264

import (
	"os"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v3/pkg/formats/rtph264"
	"github.com/pion/rtp"
)

// TestRTPChainDecodable 验证 解析器 → RTP编码 → RTP解码 链路输出可解码
func TestRTPChainDecodable(t *testing.T) {
	data, err := os.ReadFile("/tmp/multislice.h264")
	if err != nil {
		t.Skip("test stream not available:", err)
	}
	p := NewParser(25)
	var frames []*Frame
	chunk := 4096
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		frames = append(frames, p.Write(data[i:end])...)
	}
	frames = append(frames, p.Finish()...)
	if len(frames) < 70 {
		t.Fatalf("frames too few: %d", len(frames))
	}

	// RTP 编码（与引擎 pipeline 相同参数）
	enc := &rtph264.Encoder{PayloadType: 96, PacketizationMode: 1}
	if err := enc.Init(); err != nil {
		t.Fatal(err)
	}
	// RTP 解码
	dec := &rtph264.Decoder{}
	if err := dec.Init(); err != nil {
		t.Fatal(err)
	}

	var out []byte
	frameCount := 0
	for _, f := range frames {
		pkts, err := enc.Encode(f.NALUs, f.PTS)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		for _, pkt := range pkts {
			nalus, _, err := dec.Decode(pkt)
			if err != nil {
				if err.Error() == "need more packets" {
					continue // FU-A 分片未收齐，正常
				}
				t.Fatalf("decode pkt: %v", err)
			}
			for _, n := range nalus {
				out = append(out, 0, 0, 0, 1)
				out = append(out, n...)
			}
		}
		frameCount++
	}
	if err := os.WriteFile("/tmp/rtpchain_out.h264", out, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("encoded %d frames, decoded output %d bytes", frameCount, len(out))
	_ = time.Second
	_ = rtp.Packet{}
}
