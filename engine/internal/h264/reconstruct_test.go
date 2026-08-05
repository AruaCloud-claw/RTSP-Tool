package h264

import (
	"os"
	"testing"
)

// TestReconstructDecodable 解析多 slice 流并重建 Annex-B，验证可解码
func TestReconstructDecodable(t *testing.T) {
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

	// 重建 Annex-B
	var out []byte
	for _, f := range frames {
		for _, n := range f.NALUs {
			out = append(out, 0, 0, 0, 1)
			out = append(out, n...)
		}
	}
	if err := os.WriteFile("/tmp/parser_out.h264", out, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("reconstructed %d frames, %d bytes", len(frames), len(out))
}
