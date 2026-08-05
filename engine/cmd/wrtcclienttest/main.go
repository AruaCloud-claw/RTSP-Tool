// wrtcclienttest：pion 客户端连 WebRTC 预览，统计收到的 NAL 类型（验证 SPS/PPS 注入）
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
)

func min(a, b int) int { if a < b { return a }; return b }

func main() {
	apiBase := "http://127.0.0.1:18080"
	path := os.Args[1]

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fmt.Println("pc:", err)
		return
	}
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})

	counts := map[int]int{}
	logged := 0
	seqMin, seqMax := 65535, 0
	got78 := 0
	firstByte := map[int]int{}
	allNal := map[int]int{}
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		buf := make([]byte, 65536)
		for {
			n, _, err := track.Read(buf)
			if err != nil {
				return
			}
			pkt := buf[:n]
			if len(pkt) < 13 {
				continue
			}
			// TrackRemote.Read 返回完整 RTP 包（12 字节头 + 负载）
			payload := pkt[12:]
			if len(payload) < 1 {
				continue
			}
			firstByte[int(pkt[0])]++
			typ := payload[0] & 0x1f
			allNal[int(payload[0])]++
			seq := (int(pkt[2]) << 8) | int(pkt[3])
			if seq < seqMin { seqMin = seq }
			if seq > seqMax { seqMax = seq }
			if typ == 7 || typ == 8 {
				got78++
				fmt.Printf(">> 7/8包! seq=%d len=%d hex=% x\n", seq, len(pkt), pkt[:min(20,len(pkt))])
			}
			if seq == 13625 || seq == 13626 || seq == 13627 {
				fmt.Printf(">> 关键seq %d: nal=%d len=%d hex=% x\n", seq, typ, len(pkt), pkt[:min(20,len(pkt))])
			}
			if logged < 20 {
				logged++
				fmt.Printf(">> 包 seq=%d nal=%d len=%d\n", (int(pkt[2])<<8|int(pkt[3])), typ, len(pkt))
			}
			if typ == 24 { // STAP-A：统计内部 NAL 类型
				i := 1
				for i+2 <= len(payload) {
					sz := int(payload[i])<<8 | int(payload[i+1])
					i += 2
					if i+sz > len(payload) {
						break
					}
					nt := payload[i] & 0x1f
					counts[100+int(nt)]++ // 100+ 表示 STAP 内
					i += sz
				}
			}
			counts[int(typ)]++
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		fmt.Println("offer:", err)
		return
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		fmt.Println("setlocal:", err)
		return
	}
	<-webrtc.GatheringCompletePromise(pc)

	body, _ := json.Marshal(map[string]string{"path": path, "sdp": pc.LocalDescription().SDP})
	resp, err := http.Post(apiBase+"/api/v1/webrtc/offer", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Println("post:", err)
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Println("offer failed:", resp.StatusCode, string(rb))
		return
	}
	var ans struct {
		SDP string `json:"sdp"`
	}
	json.Unmarshal(rb, &ans)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
		fmt.Println("setremote:", err)
		return
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(5 * time.Second)
	fmt.Printf(">> NAL 统计: %v\n", counts)
	fmt.Printf(">> seq范围: %d ~ %d, 7/8包: %d\n", seqMin, seqMax, got78)
	fmt.Printf(">> 首字节分布: %v\n", firstByte)
	fmt.Printf(">> payload首字节分布: %v\n", allNal)
	pc.Close()
}
