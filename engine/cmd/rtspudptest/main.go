// rtspudptest：独立 UDP socket 验证服务器是否发送 RTP
package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/bluenviron/gortsplib/v3"
	gurl "github.com/bluenviron/gortsplib/v3/pkg/url"
)

func main() {
	urlStr := os.Args[1]
	u, _ := gurl.Parse(urlStr)

	// 独立 UDP listener（绕过 client 内部 listener）
	rtpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		fmt.Println("listen rtp:", err)
		os.Exit(1)
	}
	defer rtpConn.Close()
	rtpPort := rtpConn.LocalAddr().(*net.UDPAddr).Port
	rtcpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: rtpPort + 1})
	if err != nil {
		fmt.Println("listen rtcp:", err)
		os.Exit(1)
	}
	defer rtcpConn.Close()
	fmt.Printf("rtp port: %d, rtcp port: %d\n", rtpConn.LocalAddr().(*net.UDPAddr).Port, rtcpConn.LocalAddr().(*net.UDPAddr).Port)

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

	// 用指定端口 SETUP（UDP 模式）
	// 手动构造 Transport 头? Setup 的 rtpPort/rtcpPort 参数就是 client_port
	res, err := client.Setup(medias[0], u, rtpPort, rtcpConn.LocalAddr().(*net.UDPAddr).Port)
	if err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	fmt.Println("setup:", res.StatusCode, res.Header["Transport"])

	if _, err := client.Play(nil); err != nil {
		fmt.Println("play:", err)
		os.Exit(1)
	}
	fmt.Println("played, reading UDP 5s...")

	// 直接读自己的 UDP socket
	rtpConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 65536)
	count := 0
	for {
		n, addr, err := rtpConn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			fmt.Println("read err:", err)
			break
		}
		count++
		if count <= 3 {
			fmt.Printf("UDP pkt #%d from %s len=%d head=%x\n", count, addr, n, buf[:8])
		}
	}
	fmt.Printf("RESULT: %d UDP packets received\n", count)
}
