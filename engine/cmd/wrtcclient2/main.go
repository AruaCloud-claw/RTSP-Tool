// wrtcclient2：用 ReadRTP 全量打印收到的包（seq + NAL 类型）
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

func main() {
	apiBase := "http://127.0.0.1:18080"
	path := os.Args[1]

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fmt.Println("pc:", err)
		return
	}
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})

	var firstSSRC uint32
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if len(pkt.Payload) < 1 {
				continue
			}
			typ := pkt.Payload[0] & 0x1f
			// 全量打印 7/8 + 每 200 包打一次进度
			if firstSSRC == 0 { firstSSRC = pkt.SSRC }
			if typ == 7 || typ == 8 {
				fmt.Printf("GOT78 seq=%d nal=%d ts=%d len=%d ssrc=%d\n", pkt.SequenceNumber, typ, pkt.Timestamp, len(pkt.Payload), pkt.SSRC)
			}
			if pkt.SequenceNumber%2000 == 0 {
				fmt.Printf("PROGRESS seq=%d nal=%d\n", pkt.SequenceNumber, typ)
			}
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
			fmt.Println(">> connected")
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(8 * time.Second)
	fmt.Printf(">> done (firstSSRC=%d)\n", firstSSRC)
	pc.Close()
}
