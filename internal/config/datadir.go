package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// ResolveDataDir 返回数据目录（SQLite 数据库所在位置）。
//
// 优先级：
//  1. 显式设置 DATA_DIR（Electron 会把 userData 传进来）
//  2. 平台默认的用户数据目录
//
// 平台默认路径：
//   - macOS: ~/Library/Application Support/agent-rebuild
//   - Linux: $XDG_DATA_HOME/agent-rebuild 或 ~/.local/share/agent-rebuild
//   - Windows: %LOCALAPPDATA%\agent-rebuild
func ResolveDataDir(explicit string) (string, error) {
	if explicit != "" {
		return ensureDir(explicit)
	}

	base, err := os.UserConfigDir()
	if err != nil {
		// 极端情况（无法确定 HOME，如容器缺少 passwd 条目）。
		// 退化为当前目录，但必须喊出来：数据库里有加密的 API Key，
		// 悄悄落在工作目录可能被 git 带走。
		base = "."
		slog.Warn("无法确定用户配置目录，数据将写入当前工作目录",
			"err", err, "fallback", ".")
	}
	return ensureDir(filepath.Join(base, "agent-rebuild"))
}

func ensureDir(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir, nil
	}
	return abs, nil
}

// DBPath 返回数据库文件路径。
func DBPath(dataDir string) string {
	return filepath.Join(dataDir, "agent.db")
}

// IsDesktop 判断是否以桌面模式运行（Electron 会设置该变量）。
func IsDesktop() bool {
	return os.Getenv("AGENT_DESKTOP") == "1" || runtime.GOOS != "" && os.Getenv("WEB_DIR") != ""
}
