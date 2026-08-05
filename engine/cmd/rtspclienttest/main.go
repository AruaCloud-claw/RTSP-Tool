// rtspclienttest：gortsplib 客户端拉流测试工具（Setup 后注册 RTP 回调）
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/bluenviron/gortsplib/v3"
	"github.com/bluenviron/gortsplib/v3/pkg/formats"
	"github.com/bluenviron/gortsplib/v3/pkg/media"
	gurl "github.com/bluenviron/gortsplib/v3/pkg/url"
	"github.com/pion/rtp"
)

func main() {
	urlStr := os.Args[1]
	u, _ := gurl.Parse(urlStr)

	client := &gortsplib.Client{}
	if err := client.Start(u.Scheme, u.Host); err != nil {
		fmt.Println("start:", err)
		os.Exit(1)
	}
	defer client.Close()

	medias, _, _, err := client.Describe(u)
	if err != nil {
		fmt.Println("describe:", err)
		os.Exit(1)
	}
	if err := client.SetupAll(medias, u); err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	count := 0
	client.OnPacketRTPAny(func(m *media.Media, f formats.Format, pkt *rtp.Packet) {
		count++
	})
	if _, err := client.Play(nil); err != nil {
		fmt.Println("play:", err)
		os.Exit(1)
	}
	fmt.Println(">> playing 8s...")
	time.Sleep(8 * time.Second)
	fmt.Printf(">> RESULT: %d RTP packets\n", count)
}
