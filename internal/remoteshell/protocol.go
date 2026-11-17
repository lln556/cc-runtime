// Package remoteshell 让开发机里的 Claude Code 把 Bash 命令交给 Mac 执行。
//
// 开发机里的 remote-shell 客户端（本程序经 …/remote-shell/zsh 链接运行）作为 CLAUDE_CODE_SHELL，
// 每条命令建立一条 TCP 连接，经网关的 DIRECT tunnel 连到 Mac 上的服务
// （`cc-runtime remote-shell serve`，以 LaunchAgent 运行在使用者的 GUI 登录会话里）。
// 服务用新的进程组运行 /bin/zsh -c <命令>，把 stdout、stderr、退出码按帧发回。
// 连接断开、心跳超时或收到信号帧时，服务清理整个进程组。设计见 docs/remote-shell.md。
package remoteshell

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ProtocolVersion 在请求里携带；两端不一致时服务拒绝请求。
const ProtocolVersion = 1

// 帧类型。
const (
	FrameRequest byte = 'Q' // 容器 → Mac：Request（JSON），必须是第一帧
	FrameSignal  byte = 'S' // 容器 → Mac：信号名
	FramePing    byte = 'P' // 双向：心跳
	FrameStdout  byte = 'O' // Mac → 容器：stdout 字节
	FrameStderr  byte = 'E' // Mac → 容器：stderr 字节
	FrameExit    byte = 'X' // Mac → 容器：Exit（JSON），最后一帧
	FrameFail    byte = 'F' // Mac → 容器：Fail（JSON），最后一帧
)

const (
	// MaxFrame 是单帧内容的上限。
	MaxFrame = 1 << 20
	// ChunkSize 是输出切块的大小。
	ChunkSize = 64 << 10
)

// 心跳参数；测试里可以改小。
var (
	PingInterval = 15 * time.Second
	PeerTimeout  = 45 * time.Second
)

// Request 是一条命令的全部内容。
type Request struct {
	Version int    `json:"v"`
	Token   string `json:"token"`
	Cwd     string `json:"cwd"`
	// Command 和 Args 用 []byte：可能含非 UTF-8 字节（文件名），JSON 里以 base64 原样往返。
	Command []byte            `json:"cmd"`
	Args    [][]byte          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// CaptureCwd 为真时，服务把命令末尾写出的 cwd 读回来，放在 Exit.Cwd 里。
	CaptureCwd bool `json:"captureCwd,omitempty"`
}

// Exit 是命令的结束状态。
type Exit struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
	// Cwd 是命令结束时写出的工作目录（Request.CaptureCwd 为真且命令写出了它时才有）。
	Cwd []byte `json:"cwd,omitempty"`
}

// Fail 是服务拒绝或中止命令。
type Fail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var errFrameTooLarge = errors.New("帧过大")

// ReadFrame 读一帧。
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > MaxFrame {
		return 0, nil, fmt.Errorf("%w（%d 字节）", errFrameTooLarge, size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return header[0], payload, nil
}

// FrameWriter 串行化并发的写帧。
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Write 写一帧；内容超过 MaxFrame 时报错（输出要先按 ChunkSize 切块）。
func (f *FrameWriter) Write(kind byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return errFrameTooLarge
	}
	buffer := make([]byte, 5+len(payload))
	buffer[0] = kind
	binary.BigEndian.PutUint32(buffer[1:5], uint32(len(payload)))
	copy(buffer[5:], payload)
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.w.Write(buffer)
	return err
}

// WriteJSON 把 value 编成 JSON 写成一帧。
func (f *FrameWriter) WriteJSON(kind byte, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return f.Write(kind, data)
}

// WriteChunks 把输出按 ChunkSize 切块写出。
func (f *FrameWriter) WriteChunks(kind byte, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), ChunkSize)
		if err := f.Write(kind, data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
