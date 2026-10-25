// Command server 启动 agent_rebuild 演示服务。
//
// 装配顺序：config → store → lock → runner → api → http.Server。
// 任何一步失败都快速失败（进程退出），避免半可用状态。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/api"
	"agent_rebuild/internal/config"
	"agent_rebuild/internal/modelconfig"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/storage/sqlite"
	"agent_rebuild/internal/tool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

// defaultWebDir 构建时通过 -ldflags "-X main.defaultWebDir=web/dist" 注入，
// 用于让单一二进制默认托管已构建的前端产物。
var defaultWebDir string

// ReadyMarker 是「服务已就绪」的机器可读前缀。
// 桌面壳（Electron）读取 stdout 中的该行来获知真实监听地址（PORT=0 时为动态端口）。
// 形如：AGENT_READY http://localhost:52341
const ReadyMarker = "AGENT_READY "

func run() error {
	// 开发期便利：存在 .env 时自动加载（已存在的环境变量优先）。
	if n := config.LoadDotEnv(".env"); n > 0 {
		fmt.Fprintf(os.Stderr, "已从 .env 加载 %d 项配置\n", n)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogger(cfg.LogLevel)

	// 未显式指定 WEB_DIR 时，若构建时注入了默认前端目录且该目录存在，则自动启用静态托管。
	if cfg.WebDir == "" && defaultWebDir != "" {
		if info, statErr := os.Stat(defaultWebDir); statErr == nil && info.IsDir() {
			cfg.WebDir = defaultWebDir
		}
	}

	store, runner, models, closer, err := buildDataLayer(cfg)
	if err != nil {
		return err
	}
	// 确保数据库在进程退出前被正确关闭（否则 SQLite 的 WAL 可能未 checkpoint，
	// 导致重启后数据丢失——这是实测发现的真实问题）。
	defer func() {
		if closer != nil {
			if closeErr := closer(); closeErr != nil {
				slog.Error("关闭数据库失败", "err", closeErr)
			}
		}
	}()
	locks := agent.NewLocks()

	srv := api.NewServer(cfg, store, runner, locks)

	// 工具系统：默认启用只读+写文件，命令执行需显式开启。
	registry := tool.DefaultRegistry(tool.Options{
		Workspace:     cfg.ToolWorkspace,
		EnableCommand: cfg.EnableCommandTool,
	})
	srv.SetTools(registry, cfg.SystemPrompt, cfg.MaxToolSteps, cfg.ToolTimeout)
	// 注入模型配置存储 + Runner 重建器（支持运行时多模型切换）。
	if models != nil {
		srv.SetModelConfigStore(models, func(mc *modelconfig.Config) (agent.Runner, error) {
			return buildRunnerFromModelConfig(cfg, mc)
		})
	}

	// 单端口部署：托管前端静态产物（存在时才启用）。
	srv.ServeWeb(cfg.WebDir)

	httpServer := &http.Server{
		Handler: srv.Handler(),
		// SSE 长连接：WriteTimeout 必须为 0，否则流会被强制切断。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	// 显式 Listen：PORT=0 时由内核分配空闲端口，便于桌面壳（Electron）
	// 避免与用户本机已占用端口冲突，并把真实端口回传给调用方。
	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("监听端口 %s 失败: %w", cfg.Port, err)
	}
	actualPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	addr := "http://localhost:" + actualPort

	// ctx 在收到 SIGINT/SIGTERM 时取消。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 父进程守护：桌面壳（Electron）以 --parent-pid=<pid> 启动本进程，
	// 若父进程被强制终止（来不及发信号），本进程会自行退出，
	// 避免成为孤儿进程长期占用端口。
	if ppid := parentPIDFromArgs(os.Args); ppid > 0 {
		go watchParent(ppid, stop)
		slog.Info("已启用父进程守护", "parent_pid", ppid)
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("服务启动",
			"addr", addr,
			"port", actualPort,
			"env", cfg.Env,
			"provider", cfg.LLMProvider,
			"model", cfg.LLMModel,
			"store", cfg.StoreDriver,
			"web_dir", cfg.WebDir,
		)
		// 机器可读的就绪行：桌面壳（Electron）据此得知真实端口，
		// 普通运行时它只是一行普通输出，不影响人工阅读。
		fmt.Printf("%s%s\n", ReadyMarker, addr)
		os.Stdout.Sync()
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	case <-ctx.Done():
		slog.Info("收到退出信号，开始优雅关闭")
	}

	// 顺序：先中断所有生成，让 handler 有机会把状态落库，再关闭连接。
	srv.CancelAllRuns()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅关闭失败: %w", err)
	}
	srv.Wait()
	slog.Info("服务已退出")
	return nil
}

// buildDataLayer 按驱动装配存储层与执行层。
//
// 返回值 closer 用于在进程退出时释放资源（关闭数据库）。
// 忘记调用它会丢数据：SQLite 的 WAL 需要 checkpoint 才能合并到主库。
func buildDataLayer(cfg *config.Config) (session.Store, agent.Runner, modelconfig.Store, func() error, error) {
	switch cfg.StoreDriver {
	case "memory", "":
		// 内存模式：不支持持久化模型配置，退化为环境变量配置。
		runner, err := buildRunnerFromConfig(cfg)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		slog.Warn("使用内存存储：重启后会话与模型配置都会丢失")
		return session.NewMemoryStore(cfg.MaxMessagesPerSession), runner, nil, nil, nil

	case "sqlite":
		dataDir, err := config.ResolveDataDir(cfg.DataDir)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("解析数据目录失败: %w", err)
		}
		dbPath := config.DBPath(dataDir)
		db, err := sqlite.Open(dbPath)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		slog.Info("SQLite 已就绪", "path", dbPath, "data_dir", dataDir)

		// 加密密钥派生自数据目录绝对路径：
		// 数据库被拷到其他机器/其他用户目录后无法解密 API Key。
		models := modelconfig.NewSQLStore(db.SQL(), dataDir)

		runner, err := resolveRunner(cfg, models)
		if err != nil {
			_ = db.Close()
			return nil, nil, nil, nil, err
		}
		return sqlite.NewSessionStore(db, cfg.MaxMessagesPerSession), runner, models, db.Close, nil

	default:
		return nil, nil, nil, nil, fmt.Errorf(
			"不支持的 STORE_DRIVER=%q（可选 memory / sqlite）", cfg.StoreDriver)
	}
}

// resolveRunner 优先使用数据库中的默认模型配置；没有则回退到环境变量。
//
// 这样用户可以在界面上配置模型（存库），也保留了 .env 的老路子。
func resolveRunner(cfg *config.Config, models modelconfig.Store) (agent.Runner, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	def, err := models.GetDefault(ctx)
	if err == nil && def != nil {
		slog.Info("使用数据库中的默认模型配置",
			"name", def.Name, "protocol", def.Protocol,
			"provider", def.Provider, "model", def.Model,
			"has_key", def.HasAPIKey)
		return buildRunnerFromModelConfig(cfg, def)
	}
	if err != nil && !errors.Is(err, modelconfig.ErrNoDefault) {
		slog.Warn("读取默认模型配置失败，回退到环境变量", "err", err)
	}

	slog.Info("数据库中无模型配置，使用环境变量配置")
	return buildRunnerFromConfig(cfg)
}

// buildRunnerFromModelConfig 用一条模型配置构造 Runner。
func buildRunnerFromModelConfig(cfg *config.Config, mc *modelconfig.Config) (agent.Runner, error) {
	baseURL := mc.BaseURL
	model := mc.Model
	systemPrompt := mc.SystemPrompt

	// 未填地址时套用 provider 预设。
	if baseURL == "" && mc.Provider != "" {
		if spec, perr := config.NormalizeProvider(mc.Provider); perr == nil {
			baseURL = spec.BaseURL
			if model == "" {
				model = spec.DefaultModel
			}
		}
	}

	switch mc.Protocol {
	case config.ProtocolMock:
		return agent.NewMockRunner(mockModel(model)), nil
	case config.ProtocolAnthropic:
		return agent.NewAnthropicRunner(baseURL, mc.APIKey, model, systemPrompt), nil
	case config.ProtocolOpenAI:
		return agent.NewLLMRunner(baseURL, mc.APIKey, model, systemPrompt), nil
	default:
		return nil, fmt.Errorf("模型配置 %q 使用了不支持的协议 %q", mc.Name, mc.Protocol)
	}
}

func mockModel(model string) string {
	if model == "" {
		return "mock-echo"
	}
	return model
}

// buildRunnerFromConfig 按环境变量构造 Runner（旧路径，保持兼容）。
func buildRunnerFromConfig(cfg *config.Config) (agent.Runner, error) {
	// 按「协议」选择执行器，而不是按厂商。厂商只决定端点与默认模型。
	switch cfg.LLMProtocol {
	case config.ProtocolMock:
		return agent.NewMockRunner(cfg.LLMModel), nil
	case config.ProtocolAnthropic:
		return agent.NewAnthropicRunner(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.SystemPrompt), nil
	case config.ProtocolOpenAI:
		return agent.NewLLMRunner(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.SystemPrompt), nil
	default:
		return nil, fmt.Errorf("不支持的协议 %q", cfg.LLMProtocol)
	}
}

func setupLogger(level string) {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	slog.SetDefault(slog.New(h))
}
