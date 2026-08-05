// extsubtest：临时调试 — 验证外部推流的 RTP 订阅是否收到包
package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/pion/rtp"

	"rtsp-engine/internal/rtsp"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	srv := rtsp.New(":18554", "", "", "", "", logger)
	if err := srv.Start(); err != nil {
		fmt.Println("start:", err)
		os.Exit(1)
	}
	defer srv.Close()

	// 等 ffmpeg 推流进来
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if paths := srv.ExternalPaths(); len(paths) > 0 {
			fmt.Println("external path:", paths[0])
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(srv.ExternalPaths()) == 0 {
		fmt.Println("FAIL: no external push detected")
		return
	}
	path := srv.ExternalPaths()[0]
	sps, pps, ok := srv.ExternalSPSPPS(path)
	fmt.Printf("SPSPPS: ok=%v sps=%d pps=%d\n", ok, len(sps), len(pps))

	count := 0
	unsub, err := srv.SubscribeRTP(path, func(pkt *rtp.Packet) { count++ })
	if err != nil {
		fmt.Println("subscribe err:", err)
		return
	}
	defer unsub()

	time.Sleep(4 * time.Second)
	fmt.Printf("RESULT: %d RTP packets in 4s (订阅链路 %s)\n", count, map[bool]string{true: "正常", false: "异常"}[count > 0])
}
