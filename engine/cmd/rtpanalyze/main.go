// rtpanalyze：临时调试工具 — 抓取 RTP 包并打印 NAL 类型/时间戳
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/bluenviron/gortsplib/v3"
	"github.com/bluenviron/gortsplib/v3/pkg/formats"
	"github.com/bluenviron/gortsplib/v3/pkg/formats/rtph264"
	"github.com/bluenviron/gortsplib/v3/pkg/media"
	gurl "github.com/bluenviron/gortsplib/v3/pkg/url"
	"github.com/pion/rtp"
)

func nalType(p *rtp.Packet) string {
	if len(p.Payload) < 2 {
		return "short"
	}
	t := p.Payload[0] & 0x1f
	switch {
	case t == 24: // STAP-A
		if len(p.Payload) < 3 {
			return "stap? "
		}
		nal := p.Payload[2]
		return fmt.Sprintf("STAP(nal%d)", nal&0x1f)
	case t == 28: // FU-A
		nal := p.Payload[1] & 0x1f
		s := (p.Payload[1] >> 7) & 1
		e := (p.Payload[1] >> 6) & 1
		return fmt.Sprintf("FU-A(nal%d %s%s)", nal, map[byte]string{1: "S", 0: "-"}[s], map[byte]string{1: "E", 0: "-"}[e])
	default:
		return fmt.Sprintf("nal%d", t)
	}
}

func dumpSTAP(p *rtp.Packet) string {
	if len(p.Payload) < 3 || p.Payload[0]&0x1f != 24 {
		return ""
	}
	var types []byte
	i := 1
	for i+2 <= len(p.Payload) {
		sz := int(p.Payload[i])<<8 | int(p.Payload[i+1])
		i += 2
		if i+sz > len(p.Payload) {
			break
		}
		types = append(types, p.Payload[i]&0x1f)
		i += sz
	}
	return fmt.Sprintf(" STAP内NAL: %v", types)
}

func main() {
	urlStr := os.Args[1]
	u, _ := gurl.Parse(urlStr)
	tcp := gortsplib.TransportTCP
	client := &gortsplib.Client{Transport: &tcp}
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
	seen := 0
	client.OnPacketRTPAny(func(m *media.Media, f formats.Format, pkt *rtp.Packet) {
		if seen < 300 {
			m := "-"
			if pkt.Marker {
				m = "M"
			}
			fmt.Printf("#%02d ts=%-10d %-4s %s len=%d%s\n",
				seen, pkt.Timestamp, m, nalType(pkt), len(pkt.Payload), dumpSTAP(pkt))
		}
		seen++
	})
	if _, err := client.Play(nil); err != nil {
		fmt.Println("play:", err)
		os.Exit(1)
	}
	time.Sleep(6 * time.Second)
	client.Close()
	fmt.Printf(">> total %d packets\n", seen)
	_ = rtph264.Encoder{}
}
