// Package ffmpeg FFmpeg 子进程管理
package ffmpeg

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Runner 管理 FFmpeg 子进程
type Runner struct {
	mu      sync.Mutex
	path    string
	procs   map[string]*exec.Cmd
	stdins  map[string]io.WriteCloser
	logf    func(streamID, line string)
	onExit  func(streamID string, err error)
}

// NewRunner 创建进程管理器
// path: ffmpeg 可执行文件路径（空则从 PATH 查找）
// logf: 日志回调；onExit: 进程退出回调（可用于看门狗重启）
func NewRunner(path string, logf func(streamID, line string), onExit func(streamID string, err error)) *Runner {
	return &Runner{
		path:   path,
		procs:  make(map[string]*exec.Cmd),
		stdins: make(map[string]io.WriteCloser),
		logf:   logf,
		onExit: onExit,
	}
}

// Start 启动一个 FFmpeg 进程
func (r *Runner) Start(id string, args []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.procs[id]; exists {
		return fmt.Errorf("process %s already running", id)
	}

	cmd := exec.Command(r.path, args...)
	// 工作目录固定为 ffmpeg 所在目录（避免继承无效 cwd 导致 spawn 失败）
	if abs, err := filepath.Abs(r.path); err == nil {
		cmd.Dir = filepath.Dir(abs)
	}

	// stdin 管道必须在 Start 前建立（用于优雅退出 'q' 指令）
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	r.procs[id] = cmd
	r.stdins[id] = stdin

	// 日志转发
	go r.forwardLogs(id, stdout, stderr)

	// 进程退出监控
	go func() {
		err := cmd.Wait()
		r.mu.Lock()
		delete(r.procs, id)
		delete(r.stdins, id)
		r.mu.Unlock()
		if r.logf != nil {
			if err != nil {
				r.logf(id, fmt.Sprintf("ffmpeg exited: %v", err))
			} else {
				r.logf(id, "ffmpeg exited (clean)")
			}
		}
		if r.onExit != nil {
			r.onExit(id, err)
		}
	}()

	return nil
}

// Stop 停止进程：先向 stdin 发 'q' 优雅退出，超时后强杀
func (r *Runner) Stop(id string, wait time.Duration) error {
	r.mu.Lock()
	cmd, ok := r.procs[id]
	stdin := r.stdins[id]
	r.mu.Unlock()
	if !ok {
		return nil // 已退出
	}

	if stdin != nil {
		io.WriteString(stdin, "q")
	}

	// 等待优雅退出，超时强杀
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(wait):
		cmd.Process.Kill()
		<-done
		return fmt.Errorf("process %s killed after timeout", id)
	}
}

// StopAll 停止所有进程
func (r *Runner) StopAll(wait time.Duration) {
	r.mu.Lock()
	ids := make([]string, 0, len(r.procs))
	for id := range r.procs {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.Stop(id, wait)
	}
}

// IsRunning 查询进程状态
func (r *Runner) IsRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.procs[id]
	return ok
}

func (r *Runner) forwardLogs(id string, stdout, stderr io.Reader) {
	line := func(rd io.Reader) {
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			if r.logf != nil {
				r.logf(id, sc.Text())
			}
		}
	}
	go line(stdout)
	line(stderr)
}
