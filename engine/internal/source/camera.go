// 摄像头源：FFmpeg dshow (Windows) / V4L2 (Linux) 采集命令构建
package source

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// CameraSourceArgs 摄像头源参数
type CameraSourceArgs struct {
	FFmpegPath string // ffmpeg 可执行路径
	Device     string // Windows: DirectShow 设备名；Linux: /dev/videoX
	Width      int    // 0 = 默认
	Height     int    // 0 = 默认
	Framerate  int    // 0 = 默认
}

// SanitizeDshowDevice 去除浏览器枚举附加的 USB ID 后缀 "(vvvv:pppp)"
// 例："Web Camera (32e6:d412)" → "Web Camera"
// 原因：dshow 设备名中冒号是保留分隔符（video=设备名:audio=设备名），
// Chromium 附加的 VID:PID 后缀会让 ffmpeg dshow 解析报 "Malformed dshow input string"
func SanitizeDshowDevice(name string) string {
	re := regexp.MustCompile(`^(.*?)\s*\([0-9a-fA-F]{4}:[0-9a-fA-F]{2,4}\)\s*$`)
	if m := re.FindStringSubmatch(name); m != nil {
		return strings.TrimSpace(m[1])
	}
	return name
}

// BuildCameraCommand 构建摄像头采集 FFmpeg 命令
// 摄像头输出原始帧（YUYV/MJPEG），必须转码为 H.264：
//   - Windows: -f dshow -i video="设备名"
//   - Linux:   -f v4l2 -i /dev/videoX
// 转码参数：libx264 ultrafast + zerolatency（低延迟），固定 GOP
func BuildCameraCommand(args CameraSourceArgs) ([]string, error) {
	if args.Device == "" {
		return nil, fmt.Errorf("摄像头设备名为空")
	}

	cmd := []string{}
	isWindows := strings.HasPrefix(args.Device, "video=") ||
		!strings.HasPrefix(args.Device, "/dev/")

	if isWindows {
		// Windows DirectShow（先去除 Chromium 附加的 USB ID 后缀，避免冒号破坏 dshow 解析）
		dev := SanitizeDshowDevice(strings.TrimPrefix(args.Device, "video="))
		cmd = append(cmd, "-f", "dshow")
		if args.Width > 0 && args.Height > 0 {
			cmd = append(cmd, "-video_size", fmt.Sprintf("%dx%d", args.Width, args.Height))
		}
		if args.Framerate > 0 {
			cmd = append(cmd, "-framerate", fmt.Sprintf("%d", args.Framerate))
		}
		cmd = append(cmd, "-i", "video="+dev)
	} else {
		// Linux V4L2
		cmd = append(cmd, "-f", "v4l2")
		if args.Width > 0 && args.Height > 0 {
			cmd = append(cmd, "-video_size", fmt.Sprintf("%dx%d", args.Width, args.Height))
		}
		if args.Framerate > 0 {
			cmd = append(cmd, "-framerate", fmt.Sprintf("%d", args.Framerate))
		}
		cmd = append(cmd, "-i", args.Device)
	}

	// 转码为 H.264（低延迟参数）
	cmd = append(cmd,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-g", "50",
		"-f", "h264",
		"pipe:1",
	)
	return cmd, nil
}

// EnumerateCameras 枚举系统摄像头
// Windows: ffmpeg -list_devices true -f dshow -i dummy
// Linux:   /dev/video* 设备列表
// 返回设备名列表
func EnumerateCameras(ffmpegPath string) ([]string, error) {
	out := make([]string, 0)

	// Linux: 直接扫描 /dev/video*
	if entries, err := os.ReadDir("/dev"); err == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, "video") {
				out = append(out, "/dev/"+name)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	// Windows: 通过 ffmpeg 枚举 dshow 设备
	cmd := exec.Command(ffmpegPath, "-list_devices", "true", "-f", "dshow", "-i", "dummy")
	output, _ := cmd.CombinedOutput()
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// 格式: [dshow @ ...]  "设备名" (video)
		if strings.Contains(trimmed, "(video)") || strings.Contains(trimmed, "(Video)") {
			start := strings.Index(trimmed, "\"")
			if start >= 0 {
				rest := trimmed[start+1:]
				end := strings.Index(rest, "\"")
				if end > 0 {
					out = append(out, rest[:end])
				}
			}
		}
	}
	return out, nil
}
