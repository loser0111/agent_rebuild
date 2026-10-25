package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// ID 生成：前缀 + 毫秒时间戳（base36）+ 8 字节随机，保证可排序且无外部依赖。
//
//	sess_m4k2p0x1ab12cd34
//	msg_m4k2p0x1ab12cd34
func newID(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 失败属于系统级异常，退化为纳秒计数避免返回空 ID。
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s_%s%s", prefix, base36(time.Now().UnixMilli()), hex.EncodeToString(buf[:4]))
}

const digits = "0123456789abcdefghijklmnopqrstuvwxyz"

func base36(n int64) string {
	if n == 0 {
		return "0"
	}
	var out [16]byte
	i := len(out)
	for n > 0 {
		i--
		out[i] = digits[n%36]
		n /= 36
	}
	return string(out[i:])
}

// NewSessionID 暴露给需要预生成 ID 的调用方（如 api 层）。
func NewSessionID() string { return newID("sess") }

// NewMessageID 同上。
func NewMessageID() string { return newID("msg") }
