package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setEnv 设置临时环境变量，测试结束自动还原。
func setEnv(t *testing.T, key, value string) {
	t.Helper()
	old, existed := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("Setenv: %v", err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, old)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func TestLoadDefaults(t *testing.T) {
	// 清空所有相关变量，验证零配置可用。
	for _, k := range []string{
		"PORT", "APP_ENV", "CORS_ORIGINS", "STORE_DRIVER", "LLM_PROVIDER",
		"LLM_MODEL", "LLM_BASE_URL", "OPENAI_API_KEY", "SSE_PING_INTERVAL",
		"RUN_TIMEOUT", "LOG_LEVEL", "SYSTEM_PROMPT",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != "8080" {
		t.Fatalf("Port = %q", c.Port)
	}
	if c.LLMProvider != "mock" {
		t.Fatalf("LLMProvider = %q，默认应为 mock（零配置可跑）", c.LLMProvider)
	}
	if c.LLMModel != "mock-echo" {
		t.Fatalf("mock 模式下模型名应为 mock-echo，得到 %q", c.LLMModel)
	}
	if c.SSEPingInterval != 15*time.Second {
		t.Fatalf("SSEPingInterval = %v", c.SSEPingInterval)
	}
	if c.RunTimeout != 180*time.Second {
		t.Fatalf("RunTimeout = %v", c.RunTimeout)
	}
	if len(c.CORSOrigins) == 0 {
		t.Fatal("CORSOrigins 不应为空")
	}
	if !c.IsDev() {
		t.Fatal("默认 APP_ENV 应为 dev")
	}
}

func TestLoadOpenAIRequiresKey(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "openai")
	_ = os.Unsetenv("OPENAI_API_KEY")

	if _, err := Load(); err == nil {
		t.Fatal("provider=openai 且无 Key 时应快速失败")
	}
}

func TestLoadOpenAIWithKey(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("LLM_MODEL", "gpt-4o-mini")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LLMProvider != "openai" || c.LLMAPIKey != "sk-test" {
		t.Fatalf("配置错误: %+v", c)
	}
	if c.LLMModel != "gpt-4o-mini" {
		t.Fatalf("非 mock 模式不应覆盖模型名: %q", c.LLMModel)
	}
}

func TestLoadUnknownProvider(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "gemini")
	if _, err := Load(); err == nil {
		t.Fatal("未知 provider 应报错")
	}
}

func TestLoadInvalidNumbers(t *testing.T) {
	t.Setenv("MAX_MESSAGES_PER_SESSION", "0")
	if _, err := Load(); err == nil {
		t.Fatal("非正数消息上限应报错")
	}
}

func TestEnvParsing(t *testing.T) {
	t.Setenv("CORS_ORIGINS", " http://a.com , ,http://b.com ")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.CORSOrigins) != 2 || c.CORSOrigins[0] != "http://a.com" || c.CORSOrigins[1] != "http://b.com" {
		t.Fatalf("CORSOrigins 解析错误: %v", c.CORSOrigins)
	}

	// 非法 duration 回退默认值，不 panic。
	t.Setenv("RUN_TIMEOUT", "not-a-duration")
	c2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c2.RunTimeout != 180*time.Second {
		t.Fatalf("非法 duration 应回退默认值，得到 %v", c2.RunTimeout)
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# 注释行
PORT=9999
export APP_ENV=prod
QUOTED="带引号的值"
EMPTY=
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 确保目标变量未被外部设置。
	for _, k := range []string{"PORT", "APP_ENV", "QUOTED", "EMPTY"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	n := LoadDotEnv(path)
	if n < 3 {
		t.Fatalf("加载了 %d 项，期望 >= 3", n)
	}
	if got := os.Getenv("PORT"); got != "9999" {
		t.Fatalf("PORT = %q", got)
	}
	if got := os.Getenv("APP_ENV"); got != "prod" {
		t.Fatalf("APP_ENV = %q（应支持 export 前缀）", got)
	}
	if got := os.Getenv("QUOTED"); got != "带引号的值" {
		t.Fatalf("QUOTED = %q（应去掉引号）", got)
	}
}

// 已存在的环境变量必须优先于 .env（.env 只作为本地默认值）。
func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("REAL_KEY=from-file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("REAL_KEY", "from-env")

	LoadDotEnv(path)
	if got := os.Getenv("REAL_KEY"); got != "from-env" {
		t.Fatalf("已存在的环境变量被 .env 覆盖了: %q", got)
	}
}

// 文件不存在不算错误。
func TestLoadDotEnvMissingFile(t *testing.T) {
	if n := LoadDotEnv(filepath.Join(t.TempDir(), "nope.env")); n != 0 {
		t.Fatalf("缺失文件应返回 0，得到 %d", n)
	}
}
