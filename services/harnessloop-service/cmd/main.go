package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/harnessclient"
	"agent-platform/pkg/harnessloop/compaction"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/pkg/harnessloop/policy/sandbox"
	"agent-platform/pkg/harnessloop/retry"
	"agent-platform/pkg/harnessloop/tracing"
	"agent-platform/pkg/llm"
	"agent-platform/pkg/observability"
	codeindexpb "agent-platform/pkg/pb/codeindex"
	harnesspb "agent-platform/pkg/pb/harnessloop"
	mcppb "agent-platform/pkg/pb/mcp"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/config"
	"agent-platform/services/harnessloop-service/internal/handler"
	"agent-platform/services/harnessloop-service/internal/metrics"
	"agent-platform/services/harnessloop-service/internal/repository"
	"agent-platform/services/harnessloop-service/internal/service"
)

type eventSink struct{ repo *repository.EventRepo }

func (s eventSink) Append(ctx context.Context, events []*event.Event) error {
	_, err := s.repo.Append(ctx, "", "", events)
	return err
}

func main() {
	// 启动期日志同时写 stdout 与 stderr（T-P2-35/T-P2-36）。日志默认只走 stderr：
	// 由 dev-up.sh、devstack.Ensure 或容器 supervisor 拉起时，只捕获 stdout 的一方
	// 会把"启动失败"看成一秒退出、什么也没打印——所谓"静默 exit(1)"就是这么来的。
	// 双写让任何一侧的抓取者都拿得到原因，且不改变日志格式。
	log.SetOutput(io.MultiWriter(os.Stdout, os.Stderr))

	var (
		configPath = flag.String("config", "config.yaml", "path to config file")
		local      = flag.Bool("local", false, "bind to 127.0.0.1 only")
	)
	flag.Parse()

	os.Exit(run(*configPath, *local))
}

// run is the whole startup, split out of main so every failure returns a
// category-coded exit status instead of calling log.Fatalf (T-P2-35 item 2: a
// wrapper must be able to tell panic / config / database / listen apart).
func run(configPath string, local bool) (code int) {
	reporter := newExitReporter(exitStatePath())
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		pe := service.NewPanicError("main", rec)
		log.Printf("harnessloop: recovered panic during startup\n%s", pe.Detail())
		code = reporter.Fail(catPanic, pe)
	}()

	cfg, err := config.Load(configPath)
	if err != nil {
		return reporter.Fail(catConfigLoad, err)
	}
	if local {
		cfg.SetLocalOnly()
	} else if addr := os.Getenv("HARNESSLOOP_ADDRESS"); addr != "" {
		cfg.Server.Address = addr
	}
	if err := cfg.Validate(); err != nil {
		return reporter.Fail(catConfigInvalid, err)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Tracing and metrics share the OTel pipeline (design 10.1/10.3). Metrics are
	// also exposed directly on /metrics so a scrape works without a collector.
	if tp, err := observability.InitServiceTracing("harnessloop-service"); err != nil {
		log.Printf("harnessloop: otel init failed: %v", err)
	} else if tp != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tp.Shutdown(ctx)
		}()
	}
	metricsHarness := metrics.NewHarness()
	metricsAddr := os.Getenv("HARNESSLOOP_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9108"
	}
	if metricsAddr != "off" {
		// Registry.Serve swallows ListenAndServe's error inside its goroutine, so a
		// busy metrics port is invisible today. Probe it first and say so out loud
		// (T-P2-35: 端口冲突必须可判，而不是静默丢一个监听)。
		if probeErr := probeListen(metricsAddr); probeErr != nil {
			log.Printf("harnessloop: metrics %s unavailable (%v); /metrics stays dark", metricsAddr, probeErr)
		} else if srv := metricsHarness.Reg.Serve(metricsAddr); srv != nil {
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			}()
			log.Printf("harnessloop: metrics on %s/metrics", metricsAddr)
		}
	}

	db, err := pgindex.Open(rootCtx, pgindex.Config{
		DSN:                cfg.Postgres.DSN,
		MaxConns:           cfg.Postgres.MaxConns,
		MinConns:           1,
		ConnectTimeout:     5 * time.Second,
		SlowQueryThreshold: time.Duration(cfg.Postgres.SlowQueryMs) * time.Millisecond,
	})
	if err != nil {
		return reporter.Fail(catDatabase, err)
	}
	defer db.Close()

	if err := db.Migrate(rootCtx); err != nil {
		return reporter.Fail(catMigration, err)
	}

	taskRepo := repository.NewTaskRepo(db)
	attemptRepo := repository.NewAttemptRepo(db)
	eventRepo := repository.NewEventRepo(db)
	checkpointRepo := repository.NewCheckpointRepo(db)
	acceptanceRepo := repository.NewAcceptanceRepo(db)
	subtaskRepo := repository.NewSubtaskRepo(db)
	modelSelectionRepo := repository.NewModelSelectionRepo(db)
	handoffRepo := repository.NewHandoffRepo(db)

	publisher := event.NewPublisher(eventSink{repo: eventRepo}, event.NewMemoryIdempotency(10000), event.NewMemoryBuffer())
	defer func() {
		if err := publisher.Flush(context.Background()); err != nil {
			log.Printf("harnessloop: flush pending events: %v", err)
		}
	}()

	policyEngine, err := buildPolicy(cfg)
	if err != nil {
		return reporter.Fail(catPolicy, err)
	}

	models, err := modelregistry.Load(cfg.Models.File)
	if err != nil {
		log.Printf("harnessloop: model registry fallback to seed: %v", err)
		models = modelregistry.DefaultRegistry()
	}

	retryPolicy, err := retry.LoadPolicy(cfg.Retry.File)
	if err != nil {
		log.Printf("harnessloop: retry policy fallback to defaults: %v", err)
		retryPolicy = retry.DefaultPolicy()
	}

	var stages service.StageExecutor
	agentClient, err := harnessclient.DialLazy(rootCtx, harnessclient.DefaultConfig(cfg.Agent.Target))
	if err != nil {
		log.Printf("harnessloop: agent client unavailable: %v", err)
	} else {
		defer agentClient.Close()
		stages = service.NewStageRunner(agentClient).WithEvents(publisher).WithTracer(tracing.NewTracer("harnessloop-tools"))
	}

	flags, flagErr := config.LoadFlagSet(cfg.FeatureFlags.File)
	if flagErr != nil {
		log.Printf("harnessloop: feature flags unavailable, falling back to harness.enabled: %v", flagErr)
	}
	if flags == nil {
		log.Printf("harnessloop: no feature flag file configured, using harness.enabled=%v", cfg.Harness.Enabled)
	}

	// 三条 P2 开关在这里解析成 bridge 的 enabled 位：关 = 旧行为，开 = 新链路。
	gates := resolveFeatureGates(flags)
	log.Printf("harnessloop: feature gates (tenant=%s): %s", gateTenant, gates.summary())

	var recaller service.MemoryRecaller
	if cfg.Memory.Addr != "" {
		if rc, rcErr := service.DialMemoryRecaller(rootCtx, cfg.Memory.Addr, 3*time.Second); rcErr != nil {
			log.Printf("harnessloop: memory recall unavailable, will degrade to markdown: %v", rcErr)
		} else {
			recaller = rc
			defer rc.Close()
			log.Printf("harnessloop: memory recall enabled: %s", cfg.Memory.Addr)
		}
	} else {
		log.Printf("harnessloop: memory recall disabled, will degrade to markdown")
	}

	runtime := service.NewRuntime(taskRepo, attemptRepo, stages)
	runtime.Metrics = metricsHarness
	runtime.Tracer = tracing.NewTracer("harnessloop-service")
	runtime.Events = publisher
	runtime.Checkpoints = checkpointRepo
	runtime.Policy = retryPolicy
	runtime.Injector = service.NewMemoryInjector(recaller, nil).WithEvents(publisher)
	runtime.HarnessHome = cfg.Memory.HarnessHome

	emitter := orchestrator.NewPublisherEmitter(publisher, orchestrator.EmitContext{TenantID: "platform", ProjectID: "platform"})
	modelBridge := service.NewModelBridge(gates.ModelStrategy, models, "", modelSelectionRepo, emitter)
	runtime.Models = modelBridge
	if modelBridge.Enabled() {
		if hot := modelBridge.StartHotReload(rootCtx, cfg.Models.File, modelregistry.DefaultReloadInterval, "system", service.ModelStageContext{
			TenantID: "platform", ProjectID: "platform", TaskID: "model-registry",
		}); hot != nil {
			log.Printf("harnessloop: model hot reload enabled: %s", hot.Path())
		}
		log.Printf("harnessloop: model strategy bridge enabled")
	}
	orchestratorBridge := service.NewOrchestratorBridge(gates.Orchestrator, emitter, subtaskExecutor(agentClient, cfg, metricsHarness, tracing.NewTracer("harnessloop-subtasks"))).WithSubtaskRepo(subtaskRepo)
	runtime.Orchestrator = orchestratorBridge
	if orchestratorBridge.Enabled() {
		if agentClient != nil {
			log.Printf("harnessloop: orchestrator bridge enabled (subtask executor -> %s)", cfg.Agent.Target)
		} else {
			log.Printf("harnessloop: orchestrator bridge enabled without agent-service (subtasks will not execute)")
		}
	}

	approvalGate, approvalErr := buildApprovalGate(cfg)
	if approvalErr != nil {
		log.Printf("harnessloop: approval gate unavailable, high-risk calls stay pending: %v", approvalErr)
	}
	policyBridge := service.NewPolicyBridge(gates.PolicyLayers, policyEngine, repository.NewAuditRepo(db), emitter, approvalGate)
	if sr, ok := stages.(*service.StageRunner); ok {
		sr.WithToolGate(policyBridge)
	}
	if policyBridge.Enabled() {
		if approvalGate != nil {
			log.Printf("harnessloop: policy resolver bridge enabled (approval gate -> %s)", cfg.Approval.Address)
		} else {
			log.Printf("harnessloop: policy resolver bridge enabled (approval gate not configured, high-risk calls stay pending)")
		}
	}

	if os.Getenv("HARNESSLOOP_COMPACTION_ENABLED") != "false" {
		compactionCfg, compErr := compaction.LoadConfig("")
		if compErr != nil {
			compactionCfg = compaction.DefaultConfig()
		}
		runtime.CompactionTrigger = compaction.NewTrigger(compactionCfg, modelWindowResolver{models: models})
		runtime.CompactionLimits = compaction.DefaultDegradeLimits()
		if cfg.Memory.HarnessHome != "" {
			runtime.CompactionDir = filepath.Join(cfg.Memory.HarnessHome, "tmp")
		} else {
			runtime.CompactionDir = filepath.Join(os.TempDir(), "harness", "tmp")
		}
		// HARNESSLOOP_LLM_* 是可选的三件套：校验放在 config 层，缺哪一项就点名
		// 哪一项，并且降级为"不开摘要器"而不是让启动失败（T-P2-36）。
		llmEnv, llmEnvErr := config.LoadLLMEnv(os.Getenv)
		switch {
		case llmEnvErr != nil:
			log.Printf("harnessloop: compaction summarizer disabled: %v", llmEnvErr)
		case llmEnv.Configured():
			llmClient, llmErr := llm.NewClient(llm.Config{
				Provider: "openai",
				APIKey:   llmEnv.APIKey,
				BaseURL:  llmEnv.BaseURL,
				Model:    llmEnv.Model,
			})
			if llmErr != nil {
				log.Printf("harnessloop: compaction summarizer disabled: %v", llmErr)
			} else {
				runtime.CompactionSummarizer = compaction.NewTemplateSummarizer(compaction.LLMCompleter{Client: llmClient, Model: llmEnv.Model})
				log.Printf("harnessloop: compaction summarizer enabled (%s, model=%s)", llmEnv.BaseURL, llmEnv.Model)
			}
		default:
			log.Printf("harnessloop: compaction summarizer not configured (HARNESSLOOP_LLM_* unset), degradation path L1 will be used")
		}
		log.Printf("harnessloop: compaction trigger enabled (ratio=%.2f fallback=%d)", compactionCfg.ThresholdRatio, compactionCfg.FallbackWindow)
	}
	log.Printf("harnessloop: acceptance verifier is program-only in P0 (no model verifier wired yet)")

	// 启动自检：在开监听之前把"能配但一定跑不通"的组合挡下来（T-P2-35 item 5）。
	// 致命项拒绝启动；警告项逐条打出来，不让"开关亮着但什么都不做"沉底。
	findings := service.StartupSelfCheck(service.SelfCheckInput{
		ListenAddress:       cfg.ListenAddress(),
		AgentTarget:         cfg.Agent.Target,
		MetricsAddress:      metricsAddr,
		OrchestratorEnabled: orchestratorBridge.Enabled(),
		SubtaskExecutor:     agentClient != nil,
		SubtaskRepo:         subtaskRepo != nil,
		StageRunner:         stages != nil,
		PolicyLayersEnabled: policyBridge.Enabled(),
		ApprovalGate:        approvalGate != nil,
		MemoryRecall:        recaller != nil,
	})
	for _, f := range findings {
		log.Printf("harnessloop: %s", f)
	}
	log.Printf("harnessloop: %s", service.FormatFindings(findings))
	if fatal := service.FatalFindings(findings); len(fatal) > 0 {
		return reporter.Fail(catSelfCheck, errors.New(service.FormatFindings(fatal)))
	}

	h := handler.New(handler.Deps{
		Config:          cfg,
		Tasks:           taskRepo,
		Attempts:        attemptRepo,
		Events:          eventRepo,
		Checkpoints:     checkpointRepo,
		Acceptances:     acceptanceRepo,
		Subtasks:        subtaskRepo,
		ModelSelections: modelSelectionRepo,
		Handoffs:        handoffRepo,
		Runtime:         runtime,
		Policy:          policyEngine,
		PolicyBridge:    policyBridge,
		Models:          models,
		Publisher:       publisher,
		Flags:           flags,
	})

	lis, err := net.Listen("tcp", cfg.ListenAddress())
	if err != nil {
		return reporter.Fail(catListen, fmt.Errorf("listen %s: %w", cfg.ListenAddress(), err))
	}

	indexRegistry := parser.NewRegistry()
	codeWriter := codeindex.NewCodeWriter(db, func(path string, src []byte) (codeindex.ParseResult, error) {
		return indexRegistry.Parse(path, src), nil
	}, nil, nil)
	indexService := service.NewIndexService(codeWriter)
	indexHandler := handler.NewCodeIndexHandler(indexService)

	// 一个请求里的 panic 只能死一个请求（T-P2-35）。编排开启后 handler 路径上
	// 多了规划/调度/汇聚，没有这层拦截，任何一次 nil map 都会带走整个进程。
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(recoveryUnaryInterceptor(log.Default())),
		grpc.ChainStreamInterceptor(recoveryStreamInterceptor(log.Default())),
	)
	harnesspb.RegisterHarnessLoopServiceServer(server, h)
	codeindexpb.RegisterCodeIndexServiceServer(server, indexHandler)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	reflection.Register(server)

	go func() {
		log.Printf("harnessloop-service listening on %s", cfg.ListenAddress())
		if err := server.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Printf("harnessloop: serve: %v", err)
		}
	}()

	<-rootCtx.Done()
	log.Printf("harnessloop: shutdown requested")
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)

	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Duration(cfg.Harness.GracefulTimeoutSeconds) * time.Second):
		log.Printf("harnessloop: graceful stop timeout, forcing stop")
		server.Stop()
	}
	log.Printf("harnessloop: stopped")
	return exitOK
}

// probeListen reports whether addr can be bound right now, then releases it.
func probeListen(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return lis.Close()
}

type modelWindowResolver struct {
	models *modelregistry.Registry
}

func (r modelWindowResolver) ContextWindow(model string) (int, bool) {
	m, ok := r.models.Get(model)
	if !ok {
		return 0, false
	}
	return m.ContextWindow, true
}

func buildApprovalGate(cfg config.Config) (service.ToolApprovalGate, error) {
	if strings.TrimSpace(cfg.Approval.Address) == "" {
		return nil, nil
	}
	approvalCfg, err := policy.LoadApprovalConfig(cfg.Policy.ApprovalFile)
	if err != nil {
		log.Printf("harnessloop: approval config fallback to defaults: %v", err)
		approvalCfg = policy.DefaultApprovalConfig()
	}
	clientCfg := service.ApprovalClientConfig{
		Address:      cfg.Approval.Address,
		Timeout:      time.Duration(cfg.Approval.TimeoutSeconds) * time.Second,
		PollInterval: time.Duration(cfg.Approval.PollMs) * time.Millisecond,
		Approval:     approvalCfg,
	}
	// 批准之后必须把「这一次调用」的硬凭据送到执行面，否则批准只是把工单
	// 置成 approved，mcp 仍会按 exec 白名单拒绝。密钥只在本服务里；投递走
	// mcp 的 RegisterApprovalToken。地址没配就只签发不投递（执行面会拒）。
	if issuer, err := policy.ApprovalTokenIssuerFromEnv(); err != nil {
		log.Printf("harnessloop: approval token signing disabled: %v", err)
	} else if sink, err := buildApprovalTokenSink(); err != nil {
		log.Printf("harnessloop: approval token delivery disabled: %v", err)
	} else if sink != nil {
		clientCfg.TokenIssuer = issuer
		clientCfg.TokenSink = sink
		log.Printf("harnessloop: approval token signing enabled (ttl=%s, mcp=%s)", issuer.TTL(), approvalTokenMCPAddr())
	}
	client, err := service.NewApprovalClient(clientCfg)
	if err != nil {
		return nil, err
	}
	return client, nil
}

const approvalTokenMCPAddrEnv = "HARNESSLOOP_MCP_ADDR"

func approvalTokenMCPAddr() string {
	if addr := strings.TrimSpace(os.Getenv(approvalTokenMCPAddrEnv)); addr != "" {
		return addr
	}
	return "127.0.0.1:50005"
}

// buildApprovalTokenSink 连上 mcp-service，用于注册一次性审批令牌。
// gRPC 客户端是懒连接，启动时没起 mcp 不会阻塞；注册失败会由闸门
// 按「批准未生效」处理（fail closed）。
func buildApprovalTokenSink() (policy.ApprovalTokenSink, error) {
	addr := strings.TrimSpace(os.Getenv(approvalTokenMCPAddrEnv))
	if addr == "" {
		return nil, nil
	}
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &mcpTokenSink{client: mcppb.NewMCPServiceClient(conn)}, nil
}

// mcpTokenSink 实现 policy.ApprovalTokenSink：只投递令牌，不签发、不验签。
type mcpTokenSink struct {
	client mcppb.MCPServiceClient
}

func (s *mcpTokenSink) RegisterApprovalToken(ctx context.Context, token string) error {
	if s == nil || s.client == nil {
		return errors.New("mcp approval token sink is not configured")
	}
	resp, err := s.client.RegisterApprovalToken(ctx, &mcppb.RegisterApprovalTokenRequest{Token: token})
	if err != nil {
		return err
	}
	if resp == nil || !resp.Accepted {
		reason := ""
		if resp != nil {
			reason = resp.Reason
		}
		return fmt.Errorf("mcp rejected approval token: %s", reason)
	}
	return nil
}

func subtaskExecutor(client *harnessclient.Client, cfg config.Config, harness *metrics.Harness, tracer *tracing.Tracer) service.SubtaskExecutor {
	if client == nil {
		return nil
	}
	executor := service.NewAgentSubtaskExecutor(client, service.SubtaskExecutorConfig{
		EntryAgent: cfg.Agent.EntryAgent,
		Model:      cfg.Agent.StageModel,
		Timeout:    time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
	})
	executor.Metrics = harness
	executor.Tracer = tracer
	return executor
}

func buildPolicy(cfg config.Config) (*policy.Engine, error) {
	engine := policy.NewEngine()
	if m, err := policy.LoadMatrix(cfg.Policy.MatrixFile); err == nil {
		engine.Matrix = m
	} else {
		log.Printf("harnessloop: stage matrix fallback to defaults: %v", err)
	}
	if a, err := sandbox.LoadExecAllowlist(cfg.Policy.ExecAllowFile); err == nil {
		engine.ExecAllow = a
	} else {
		log.Printf("harnessloop: exec allowlist fallback to defaults: %v", err)
	}
	return engine, nil
}
