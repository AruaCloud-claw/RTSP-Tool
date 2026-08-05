// Package config 引擎配置加载
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 引擎全局配置
type Config struct {
	// HTTP API 服务监听地址（Electron UI 通过此接口通信）
	HTTPListen string `yaml:"http_listen"`
	// 内置 RTSP 服务监听地址
	RTSPListen string `yaml:"rtsp_listen"`
	// UDP RTP/RTCP 监听地址（空 = 自动推导：RTSP 端口 - 554）
	UDPRTPAddress  string `yaml:"udp_rtp_address"`
	UDPRTCPAddress string `yaml:"udp_rtcp_address"`
	// FFmpeg 可执行文件路径（空则从 PATH 查找）
	FFmpegPath string `yaml:"ffmpeg_path"`
	// FFprobe 可执行文件路径（空则从 PATH 查找）
	FFprobePath string `yaml:"ffprobe_path"`
	// 日志级别: debug/info/warn/error
	LogLevel string `yaml:"log_level"`
	// 数据目录（快照、日志等）
	DataDir string `yaml:"data_dir"`
	// 鉴权配置
	Auth AuthConfig `yaml:"auth"`
}

// AuthConfig RTSP 拉流鉴权
type AuthConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Default 返回默认配置
func Default() *Config {
	return &Config{
		HTTPListen: "127.0.0.1:18080", // 避开 Windows HTTP.sys/WinNAT 常保留的 808x 段
		RTSPListen:     ":8554",
		UDPRTPAddress:  "",
		UDPRTCPAddress: "",
		FFmpegPath: "ffmpeg", // 空值由上层处理；默认走 PATH 查找
		FFprobePath: "ffprobe",
		LogLevel:   "info",
		DataDir:    "data",
		Auth: AuthConfig{
			Username: "",
			Password: "",
		},
	}
}

// Load 从文件加载配置，文件不存在时使用默认值
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}
