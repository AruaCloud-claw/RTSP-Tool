// Package stream 流管理器：流生命周期管理
package stream

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// SourceType 输入源类型
type SourceType string

const (
	SourceFile    SourceType = "file"     // 视频文件
	SourceCamera  SourceType = "camera"   // USB 摄像头
	SourceRTSP    SourceType = "rtsp"     // 外部 RTSP 拉流
)

// TargetType 输出目标类型
type TargetType string

const (
	TargetLocal  TargetType = "local"   // 内置 RTSP 服务
	TargetRemote TargetType = "remote"  // 外部 RTSP 服务器
)

// StreamStatus 流状态
type StreamStatus string

const (
	StatusStopped  StreamStatus = "stopped"
	StatusStarting StreamStatus = "starting"
	StatusRunning  StreamStatus = "running"
	StatusError    StreamStatus = "error"
)

// Stream 一路流的完整定义
type Stream struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	SourceType  SourceType   `json:"source_type"`
	SourceArgs  SourceArgs   `json:"source_args"`
	TargetType  TargetType   `json:"target_type"`
	TargetArgs  TargetArgs   `json:"target_args"`
	Status      StreamStatus `json:"status"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	LastError   string       `json:"last_error,omitempty"`

	// 运行时统计（由引擎更新）
	FPS       float64   `json:"fps"`
	Bitrate   int64     `json:"bitrate_kbps"`
	Clients   int       `json:"clients"`
	StartedAt time.Time `json:"started_at,omitempty"`

	mu sync.RWMutex `json:"-"`
}

// SourceArgs 输入源参数
type SourceArgs struct {
	// 文件源
	FilePath string `json:"file_path,omitempty"`
	Loop     bool   `json:"loop,omitempty"`

	// 摄像头源
	Device   string `json:"device,omitempty"`   // Windows: DirectShow 设备名
	Width    int    `json:"width,omitempty"`    // 0 = 默认
	Height   int    `json:"height,omitempty"`   // 0 = 默认
	Framerate int   `json:"framerate,omitempty"` // 0 = 默认

	// RTSP 源
	URL string `json:"url,omitempty"`
}

// TargetArgs 输出目标参数
type TargetArgs struct {
	// 内置服务路径，如 live/cam1
	Path string `json:"path,omitempty"`
	// 外部服务器完整 URL，如 rtsp://user:pass@host:554/live/x
	URL string `json:"url,omitempty"`
}

// SetStatus 线程安全更新状态
func (s *Stream) SetStatus(st StreamStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = st
	s.UpdatedAt = time.Now()
}

// Manager 流管理器
type Manager struct {
	mu      sync.RWMutex
	streams map[string]*Stream
	nextID  int
}

// NewManager 创建流管理器
func NewManager() *Manager {
	return &Manager{
		streams: make(map[string]*Stream),
	}
}

// Save 持久化所有流到 JSON 文件
func (m *Manager) Save(path string) error {
	m.mu.RLock()
	list := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		// 手动字段拷贝（避免拷贝锁；不保存运行时统计）
		cp := &Stream{
			ID:         s.ID,
			Name:       s.Name,
			SourceType: s.SourceType,
			SourceArgs: s.SourceArgs,
			TargetType: s.TargetType,
			TargetArgs: s.TargetArgs,
			Status:     s.Status,
			CreatedAt:  s.CreatedAt,
			UpdatedAt:  s.UpdatedAt,
			LastError:  s.LastError,
		}
		cp.Status = StatusStopped
		cp.FPS = 0
		cp.Bitrate = 0
		cp.Clients = 0
		list = append(list, cp)
	}
	m.mu.RUnlock()
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Load 从 JSON 文件加载流（状态重置为 stopped）
func (m *Manager) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []*Stream
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range list {
		s.Status = StatusStopped
		s.FPS = 0
		s.Bitrate = 0
		s.Clients = 0
		s.LastError = ""
		s.StartedAt = time.Time{}
		s.UpdatedAt = time.Now()
		m.streams[s.ID] = s
		if id := parseID(s.ID); id >= m.nextID {
			m.nextID = id + 1
		}
	}
	return nil
}

// parseID 从流 ID（s123）解析数字
func parseID(id string) int {
	n := 0
	for _, c := range id {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// Create 创建流（状态为 stopped，需手动 Start）
func (m *Manager) Create(s *Stream) (*Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	if s.ID == "" {
		s.ID = "s" + itoa(m.nextID)
	}
	if s.Name == "" {
		s.Name = s.ID
	}
	s.Status = StatusStopped
	s.CreatedAt = time.Now()
	s.UpdatedAt = s.CreatedAt
	m.streams[s.ID] = s
	return s, nil
}

// Get 查询流
func (m *Manager) Get(id string) (*Stream, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.streams[id]
	return s, ok
}

// List 列出所有流
func (m *Manager) List() []*Stream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		out = append(out, s)
	}
	return out
}

// Delete 删除流（需先停止）
func (m *Manager) Delete(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.streams[id]; !ok {
		return false
	}
	delete(m.streams, id)
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
