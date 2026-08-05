// pipeline 统一接口：所有输入源管道（文件/摄像头/RTSP拉流）实现该接口
package pipeline

import "github.com/pion/rtp"

// Interface 媒体管道统一接口
// 三种输入源（文件/摄像头/RTSP 拉流）对外暴露一致能力：
//   - RTP 订阅：WebRTC 预览、外部推流复用同一份 RTP 流
//   - SPS/PPS：用于 SDP 生成
//   - 生命周期：Stop / Done
//   - 统计：帧率、码率、客户端数
type Interface interface {
	// AddRTPSubscriber 注册 RTP 订阅者，返回取消函数
	AddRTPSubscriber(fn func(pkt *rtp.Packet)) func()
	// SPSPPS 返回流的参数集
	SPSPPS() ([]byte, []byte)
	// Stop 停止管道
	Stop()
	// Stats 实时统计（帧率、码率 kbps、内置服务客户端数）
	Stats() (fps float64, bitrateKbps int64, clients int)
	// Done 完成信号（管道退出时关闭）
	Done() <-chan struct{}
}

// 编译期断言：两种管道都实现 Interface
var _ Interface = (*FilePipeline)(nil)
var _ Interface = (*RTSPPullPipeline)(nil)
