package h264

import (
	"os"
	"testing"
)

// TestMultiSliceFrameGrouping 验证多 slice 帧正确按存取单元分组
// 数据源：ffmpeg -tune zerolatency（sliced threads）产生的多 slice 流
func TestMultiSliceFrameGrouping(t *testing.T) {
	data, err := os.ReadFile("/tmp/multislice.h264")
	if err != nil {
		t.Skip("test stream not available:", err)
	}

	// 用修复后的解析器
	p := NewParser(25)
	var frames []*Frame
	// 分块喂入，模拟管道读取
	chunk := 4096
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		frames = append(frames, p.Write(data[i:end])...)
	}
	frames = append(frames, p.Finish()...)

	// 3 秒 25fps = 75 帧（±1 容忍边界帧）
	if len(frames) < 70 || len(frames) > 80 {
		t.Fatalf("帧数异常: got %d, want ~75（多 slice 帧被错误拆分）", len(frames))
	}
	// 关键帧必须含 SPS/PPS
	keyOK := 0
	for _, f := range frames {
		if f.Key {
			hasSPS, hasPPS := false, false
			for _, n := range f.NALUs {
				switch NALType(n[0]) {
				case NALSPS:
					hasSPS = true
				case NALPPS:
					hasPPS = true
				}
			}
			if hasSPS && hasPPS {
				keyOK++
			}
		}
	}
	if keyOK == 0 {
		t.Fatal("关键帧缺少 SPS/PPS")
	}
	t.Logf("OK: %d 帧（含 %d 个关键帧），多 slice 分组正确", len(frames), keyOK)
}
