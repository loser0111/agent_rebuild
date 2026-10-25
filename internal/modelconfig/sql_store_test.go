package modelconfig_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"agent_rebuild/internal/modelconfig"
	"agent_rebuild/internal/storage/sqlite"
)

func newStore(t *testing.T) *modelconfig.SQLStore {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "models.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return modelconfig.NewSQLStore(db.SQL(), "test-machine-secret")
}

func validInput(name string) modelconfig.Input {
	return modelconfig.Input{
		Name:     name,
		Protocol: modelconfig.ProtocolOpenAI,
		Provider: "deepseek",
		Model:    "deepseek-chat",
		APIKey:   "sk-test-" + name,
	}
}

func TestCreateAndGet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	cfg, err := s.Create(ctx, validInput("工作用 DeepSeek"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cfg.ID == "" || !strings.HasPrefix(cfg.ID, "mc_") {
		t.Fatalf("ID 格式错误: %q", cfg.ID)
	}
	if !cfg.HasAPIKey {
		t.Fatal("HasAPIKey 应为 true")
	}
	// 第一条配置应自动成为默认。
	if !cfg.IsDefault {
		t.Fatal("首条配置应自动设为默认")
	}

	got, err := s.Get(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "工作用 DeepSeek" || got.Model != "deepseek-chat" {
		t.Fatalf("读取结果不一致: %+v", got)
	}
	// Get 用于内部调用，应能拿到解密后的密钥。
	if got.APIKey != "sk-test-工作用 DeepSeek" {
		t.Fatalf("APIKey 解密失败: %q", got.APIKey)
	}
}

// 密钥必须加密落盘，不能明文存库。
func TestAPIKeyEncryptedAtRest(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "enc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	s := modelconfig.NewSQLStore(db.SQL(), "secret-a")
	ctx := context.Background()

	secret := "sk-super-secret-value-12345"
	in := validInput("加密测试")
	in.APIKey = secret
	cfg, err := s.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 直接查库，确认没有明文。
	var stored string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT api_key_enc FROM model_configs WHERE id = ?`, cfg.ID).Scan(&stored); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if stored == "" {
		t.Fatal("密文为空")
	}
	if strings.Contains(stored, secret) {
		t.Fatal("数据库中出现了明文密钥！")
	}
	if strings.Contains(stored, "sk-") {
		t.Fatal("密文中不应出现密钥前缀明文")
	}

	// 换一个「机器密钥」应无法解密（模拟数据库被拷到别的机器）。
	otherStore := modelconfig.NewSQLStore(db.SQL(), "different-machine")
	got, err := otherStore.Get(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.HasAPIKey {
		t.Fatal("不同派生密钥不应能解密成功")
	}
	if got.APIKey != "" {
		t.Fatal("解密失败时不应返回密钥内容")
	}
}

// 列表接口绝不能返回明文密钥。
func TestListDoesNotLeakKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, validInput("列表测试")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("配置数 = %d", len(list))
	}
	if list[0].APIKey != "" {
		t.Fatal("List 不应返回明文密钥！")
	}
	if !list[0].HasAPIKey {
		t.Fatal("List 应标记存在密钥")
	}
}

func TestMultiModelSupport(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	inputs := []modelconfig.Input{
		{Name: "DeepSeek", Protocol: modelconfig.ProtocolOpenAI, Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-1"},
		{Name: "Claude", Protocol: modelconfig.ProtocolAnthropic, Provider: "anthropic", Model: "claude-3-5-sonnet-latest", APIKey: "sk-ant-2"},
		{Name: "本地 Ollama", Protocol: modelconfig.ProtocolOpenAI, Provider: "ollama", Model: "qwen2.5:7b"},
		{Name: "Mock", Protocol: modelconfig.ProtocolMock},
	}
	for _, in := range inputs {
		if _, err := s.Create(ctx, in); err != nil {
			t.Fatalf("Create(%s): %v", in.Name, err)
		}
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 4 {
		t.Fatalf("配置数 = %d，期望 4", len(list))
	}
	// 默认配置应排在首位。
	if !list[0].IsDefault {
		t.Fatal("默认配置应排在最前")
	}
	// 只有一个是默认。
	defaults := 0
	for _, c := range list {
		if c.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("默认配置数 = %d，期望恰好 1 个", defaults)
	}
}

func TestSetDefault(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	a, _ := s.Create(ctx, validInput("A"))
	b, _ := s.Create(ctx, validInput("B"))

	if !a.IsDefault {
		t.Fatal("A 应为默认（首条）")
	}
	if err := s.SetDefault(ctx, b.ID); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}

	def, err := s.GetDefault(ctx)
	if err != nil {
		t.Fatalf("GetDefault: %v", err)
	}
	if def.ID != b.ID {
		t.Fatalf("默认配置 = %q，期望 %q", def.Name, b.Name)
	}

	// A 不再是默认。
	gotA, _ := s.Get(ctx, a.ID)
	if gotA.IsDefault {
		t.Fatal("切换默认后 A 不应还是默认")
	}
}

func TestDeleteKeepsDefault(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	a, _ := s.Create(ctx, validInput("A"))
	b, _ := s.Create(ctx, validInput("B"))

	// 删除默认配置后，应自动指定新的默认。
	if err := s.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	def, err := s.GetDefault(ctx)
	if err != nil {
		t.Fatalf("删除默认后应仍有默认: %v", err)
	}
	if def.ID != b.ID {
		t.Fatalf("新默认 = %q，期望 B", def.Name)
	}
}

func TestUniqueName(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, validInput("重名")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := s.Create(ctx, validInput("重名"))
	if !errors.Is(err, modelconfig.ErrConflict) {
		t.Fatalf("err = %v，期望 ErrConflict", err)
	}
}

// 更新时 APIKey 留空表示「不改动已有密钥」。
func TestUpdatePreservesKeyWhenEmpty(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cfg, _ := s.Create(ctx, validInput("保留密钥"))

	in := validInput("保留密钥")
	in.Model = "deepseek-reasoner"
	in.APIKey = "" // 不传密钥
	updated, err := s.Update(ctx, cfg.ID, in)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Model != "deepseek-reasoner" {
		t.Fatalf("Model 未更新: %q", updated.Model)
	}
	if !updated.HasAPIKey {
		t.Fatal("空 APIKey 更新不应清除密钥")
	}
	got, _ := s.Get(ctx, cfg.ID)
	if got.APIKey == "" {
		t.Fatal("原密钥丢失")
	}
}

// 传入新密钥应替换旧密钥。
func TestUpdateReplacesKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cfg, _ := s.Create(ctx, validInput("换密钥"))

	in := validInput("换密钥")
	in.APIKey = "sk-brand-new-key"
	if _, err := s.Update(ctx, cfg.ID, in); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get(ctx, cfg.ID)
	if got.APIKey != "sk-brand-new-key" {
		t.Fatalf("密钥未替换: %q", got.APIKey)
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		in   modelconfig.Input
	}{
		{"空名称", modelconfig.Input{Protocol: modelconfig.ProtocolOpenAI, Model: "m"}},
		{"非法协议", modelconfig.Input{Name: "x", Protocol: "grpc", Model: "m"}},
		{"缺模型名", modelconfig.Input{Name: "x", Protocol: modelconfig.ProtocolOpenAI}},
		{"温度越界", func() modelconfig.Input {
			v := 5.0
			return modelconfig.Input{Name: "x", Protocol: modelconfig.ProtocolOpenAI, Model: "m", Temperature: &v}
		}()},
		{"负 max_tokens", modelconfig.Input{Name: "x", Protocol: modelconfig.ProtocolOpenAI, Model: "m", MaxTokens: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			if _, err := s.Create(context.Background(), c.in); !errors.Is(err, modelconfig.ErrInvalid) {
				t.Fatalf("err = %v，期望 ErrInvalid", err)
			}
		})
	}
}

// mock 协议不需要模型名与地址。
func TestMockProtocolNoModelRequired(t *testing.T) {
	s := newStore(t)
	cfg, err := s.Create(context.Background(), modelconfig.Input{
		Name: "本地演示", Protocol: modelconfig.ProtocolMock,
	})
	if err != nil {
		t.Fatalf("mock 协议应允许空模型名: %v", err)
	}
	if cfg.Protocol != modelconfig.ProtocolMock {
		t.Fatalf("Protocol = %q", cfg.Protocol)
	}
}

func TestCipherRoundTrip(t *testing.T) {
	c := modelconfig.NewCipher("machine-a")

	cases := []string{"sk-abc", "含中文的密钥", "", strings.Repeat("x", 500)}
	for _, plain := range cases {
		enc, err := c.Encrypt(plain)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plain, err)
		}
		got, err := c.Decrypt(enc)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != plain {
			t.Fatalf("往返不一致: %q -> %q", plain, got)
		}
	}
}

func TestCipherDifferentSecrets(t *testing.T) {
	a := modelconfig.NewCipher("machine-a")
	b := modelconfig.NewCipher("machine-b")
	enc, _ := a.Encrypt("sk-secret")
	if _, err := b.Decrypt(enc); err == nil {
		t.Fatal("不同机器密钥不应能解密")
	}
}

func TestGetDefaultWhenEmpty(t *testing.T) {
	s := newStore(t)
	_, err := s.GetDefault(context.Background())
	if !errors.Is(err, modelconfig.ErrNoDefault) {
		t.Fatalf("err = %v，期望 ErrNoDefault", err)
	}
}
