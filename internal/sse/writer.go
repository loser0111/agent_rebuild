package sse

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// ErrClosed 表示流已关闭。
var ErrClosed = errors.New("sse: writer closed")

// Writer 封装 SSE 响应写入：编码 + 立即 flush + 心跳。
// 非并发安全：约定仅由单个 handler goroutine 持有（心跳用 select 在同一个循环里发）。
type Writer struct {
	w       http.ResponseWriter
	flusher interface{ Flush() }
	mu      sync.Mutex // 防御性：即使被多 goroutine 误用也不会写坏帧
	closed  bool
	started bool // 是否已写过 retry 指令
	written uint64
}

// NewWriter 校验底层是否支持 Flush，并写好响应头。
// 调用成功后调用方应立刻开始写事件；返回 error 说明该连接无法做 SSE。
func NewWriter(w http.ResponseWriter) (*Writer, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	f, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("sse: ResponseWriter 不支持 Flush")
	}
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &Writer{w: w, flusher: f}, nil
}

// WriteEvent 编码并写入一帧，随后立即 flush。
// retry 仅在首次写入生效（见 docs/02-api-and-sse.md §3.1）。
func (w *Writer) WriteEvent(ev Event) error {
	var b strings.Builder

	if ev.Retry > 0 && !w.started {
		b.WriteString("retry: ")
		b.WriteString(strconv.Itoa(ev.Retry))
		b.WriteString("\n")
	}
	w.started = true

	if ev.ID > 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatUint(ev.ID, 10))
		b.WriteString("\n")
	}
	if ev.Name != "" {
		b.WriteString("event: ")
		b.WriteString(ev.Name)
		b.WriteString("\n")
	}
	if ev.Data != nil {
		payload, err := json.Marshal(ev.Data)
		if err != nil {
			return fmt.Errorf("sse: 序列化事件 %q 失败: %w", ev.Name, err)
		}
		// data 必须为单行 JSON，杜绝多行 data 的解析歧义。
		b.WriteString("data: ")
		b.WriteString(strings.ReplaceAll(string(payload), "\n", " "))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	return w.writeRaw(b.String())
}

// WriteComment 写一行注释（如 ": connected"），可用于即时探活。
func (w *Writer) WriteComment(text string) error {
	return w.writeRaw(": " + text + "\n\n")
}

// Ping 发送心跳事件。
func (w *Writer) Ping(data PingData) error {
	return w.WriteEvent(Event{Name: EventPing, Data: data})
}

func (w *Writer) writeRaw(s string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if _, err := w.w.Write([]byte(s)); err != nil {
		w.closed = true
		return err
	}
	w.flusher.Flush()
	w.written++
	return nil
}

// Close 标记流结束（不关闭底层连接，由 handler return 触发）。
func (w *Writer) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

// Frames 返回已写入的帧数（调试/日志用）。
func (w *Writer) Frames() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}
