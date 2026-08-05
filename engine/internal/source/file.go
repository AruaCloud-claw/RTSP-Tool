// Package source 输入源：FFmpeg 命令构建与媒体探测
package source

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
)

// MediaInfo 探测到的媒体信息
type MediaInfo struct {
	Codec     string  `json:"codec_name"`     // 如 h264
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	FPS       float64 `json:"fps"`            // 帧率
	HasVideo  bool    `json:"has_video"`
}

// Probe 用 ffmpeg -i 探测媒体文件（不依赖 ffprobe，解析 stderr 输出）
func Probe(ffmpegPath, file string) (*MediaInfo, error) {
	cmd := exec.Command(ffmpegPath, "-i", file)
	out, _ := cmd.CombinedOutput() // ffmpeg -i 信息输出在 stderr，正常返回非零
	text := string(out)

	info := &MediaInfo{}

	// 编码："Video: h264 (High) ..."
	if m := regexp.MustCompile(`Video: (\w+)`).FindStringSubmatch(text); len(m) == 2 {
		info.Codec = m[1]
		info.HasVideo = true
	}
	// 分辨率："640x360"
	if m := regexp.MustCompile(`(\d{2,5})x(\d{2,5})`).FindStringSubmatch(text); len(m) == 3 {
		info.Width, _ = strconv.Atoi(m[1])
		info.Height, _ = strconv.Atoi(m[2])
	}
	// 帧率："25 fps"
	if m := regexp.MustCompile(`([\d.]+) fps`).FindStringSubmatch(text); len(m) == 2 {
		info.FPS, _ = strconv.ParseFloat(m[1], 64)
	}
	if info.FPS <= 0 {
		info.FPS = 25
	}
	if !info.HasVideo {
		return nil, fmt.Errorf("%s 中没有检测到视频流", file)
	}
	return info, nil
}

// FileSourceArgs 文件源参数
type FileSourceArgs struct {
	FFmpegPath string // ffmpeg 可执行路径
	FilePath   string // 视频文件路径
	Loop       bool   // 是否循环
	FPS        float64 // 目标帧率（0 = 源帧率）
}

// BuildFileCommand 构建文件源 FFmpeg 命令
// 输出：stdout 裸 H.264 Annex-B 流（-f h264 -bsf:v h264_mp4toannexb）
// 说明：
//   - 仅支持 H.264 编码输入（透传模式）；其他编码走转码在 P4 支持
//   - 无音频（P1 仅视频）
func BuildFileCommand(args FileSourceArgs) ([]string, error) {
	cmd := []string{}
	if args.Loop {
		cmd = append(cmd, "-stream_loop", "-1")
	}
	cmd = append(cmd,
		"-re",                    // 实时速率
		"-i", args.FilePath,      // 输入
		"-an",                    // 丢弃音频
		"-c:v", "copy",           // 视频透传（不解码）
		"-bsf:v", "h264_mp4toannexb", // MP4(AVCC) → Annex-B
		"-f", "h264",             // 输出裸 H.264
		"pipe:1",
	)
	return cmd, nil
}

// StartFFmpeg 启动 FFmpeg 进程（阻塞读 stdout 用）
// 返回 cmd 和 stdout 管道
func StartFFmpeg(ctx context.Context, ffmpegPath string, args []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stderr = nil // 日志由调用方接管（可设置 cmd.Stderr 为自定义 writer）
	return cmd, nil
}
