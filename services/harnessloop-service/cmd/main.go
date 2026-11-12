package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
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
	"agent-platform/pkg/llm"
	codeindexpb "agent-platform/pkg/pb/codeindex"
	harnesspb "agent-platform/pkg/pb/harnessloop"
	"agent-platform/pkg/pgindex"
	"agent-platform/services/harnessloop-service/internal/config"
	"agent-platform/services/harnessloop-service/internal/handler"
	"agent-platform/services/harnessloop-service/internal/repository"
	"agent-platform/services/harnessloop-service/internal/service"
)

type eventSink struct{ repo *repository.EventRepo }

func (s eventSink) Append(ctx context.Context, events []*event.Event) error {
	_, err := s.repo.Append(ctx, "", "", events)
	return err
}

func main() {
	var (
		configPath = flag.String("config", "config.yaml", "path to config file")
		local      = flag.Bool("local", false, "bind to 127.0.0.1 only")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("harnessloop: load config: %v", err)
	}
	if *local {
		cfg.Server.Address = "127.0.0.1:50008"
	} else if addr := os.Getenv("HARNESSLOOP_ADDRESS"); addr != "" {
		cfg.Server.Address = addr
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("harnessloop: invalid config: %v", err)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := pgindex.Open(rootCtx, pgindex.Config{
		DSN:                cfg.Postgres.DSN,
		MaxConns:           cfg.Postgres.MaxConns,
		MinConns:           1,
		ConnectTimeout:     5 * time.Second,
		SlowQueryThreshold: time.Duration(cfg.Postgres.SlowQueryMs) * time.Millisecond,
	})
	if err != nil {
		log.Fatalf("harnessloop: connect postgres: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(rootCtx); err != nil {
		log.Fatalf("harnessloop: migrate: %v", err)
	}

	taskRepo := repository.NewTaskRepo(db)
	attemptRepo := repository.NewAttemptRepo(db)
	eventRepo := repository.NewEventRepo(db)
	checkpointRepo := repository.NewCheckpointRepo(db)
	acceptanceRepo := repository.NewAcceptanceRepo(db)

	publisher := event.NewPublisher(eventSink{repo: eventRepo}, event.NewMemoryIdempotency(10000), event.NewMemoryBuffer())
	defer func() {
		if err := publisher.Flush(context.Background()); err != nil {
			log.Printf("harnessloop: flush pending events: %v", err)
		}
	}()

	policyEngine, err := buildPolicy(cfg)
	if err != nil {
		log.Fatalf("harnessloop: load policy: %v", err)
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
		stages = service.NewStageRunner(agentClient).WithEvents(publisher)
	}

	flags, flagErr := config.LoadFlagSet(cfg.FeatureFlags.File)
	if flagErr != nil {
		log.Printf("harnessloop: feature flags unavailable, falling back to harness.enabled: %v", flagErr)
	}
	if flags == nil {
		log.Printf("harnessloop: no feature flag file configured, using harness.enabled=%v", cfg.Harness.Enabled)
	}

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
	runtime.Events = publisher
	runtime.Checkpoints = checkpointRepo
	runtime.Policy = retryPolicy
	runtime.Injector = service.NewMemoryInjector(recaller, nil).WithEvents(publisher)
	runtime.HarnessHome = cfg.Memory.HarnessHome

	emitter := orchestrator.NewPublisherEmitter(publisher, orchestrator.EmitContext{TenantID: "platform", ProjectID: "platform"})
	modelBridge := service.NewModelBridge(flagDefault(flags, config.FlagModelStrategyEnabled), models, "", repository.NewModelSelectionRepo(db), emitter)
	runtime.Models = modelBridge
	if modelBridge.Enabled() {
		if hot := modelBridge.StartHotReload(rootCtx, cfg.Models.File, modelregistry.DefaultReloadInterval, "system", service.ModelStageContext{
			TenantID: "platform", ProjectID: "platform", TaskID: "model-registry",
		}); hot != nil {
			log.Printf("harnessloop: model hot reload enabled: %s", hot.Path())
		}
		log.Printf("harnessloop: model strategy bridge enabled")
	}
	orchestratorBridge := service.NewOrchestratorBridge(flagDefault(flags, config.FlagOrchestratorEnabled), emitter, subtaskExecutor(agentClient, cfg))
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
	policyBridge := service.NewPolicyBridge(flagDefault(flags, config.FlagPolicyResolverEnabled), policyEngine, repository.NewAuditRepo(db), emitter, approvalGate)
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
		if baseURL := os.Getenv("HARNESSLOOP_LLM_BASE_URL"); baseURL != "" {
			llmClient, llmErr := llm.NewClient(llm.Config{
				Provider: "openai",
				APIKey:   os.Getenv("HARNESSLOOP_LLM_API_KEY"),
				BaseURL:  baseURL,
				Model:    os.Getenv("HARNESSLOOP_LLM_MODEL"),
			})
			if llmErr != nil {
				log.Printf("harnessloop: compaction summarizer disabled: %v", llmErr)
			} else {
				runtime.CompactionSummarizer = compaction.NewTemplateSummarizer(compaction.LLMCompleter{Client: llmClient, Model: os.Getenv("HARNESSLOOP_LLM_MODEL")})
				log.Printf("harnessloop: compaction summarizer enabled (%s)", baseURL)
			}
		} else {
			log.Printf("harnessloop: compaction summarizer not configured, degradation path L1 will be used")
		}
		log.Printf("harnessloop: compaction trigger enabled (ratio=%.2f fallback=%d)", compactionCfg.ThresholdRatio, compactionCfg.FallbackWindow)
	}
	log.Printf("harnessloop: acceptance verifier is program-only in P0 (no model verifier wired yet)")

	h := handler.New(handler.Deps{
		Config:       cfg,
		Tasks:        taskRepo,
		Attempts:     attemptRepo,
		Events:       eventRepo,
		Checkpoints:  checkpointRepo,
		Acceptances:  acceptanceRepo,
		Runtime:      runtime,
		Policy:       policyEngine,
		PolicyBridge: policyBridge,
		Models:       models,
		Publisher:    publisher,
		Flags:        flags,
	})

	lis, err := net.Listen("tcp", cfg.ListenAddress())
	if err != nil {
		log.Fatalf("harnessloop: listen %s: %v", cfg.ListenAddress(), err)
	}

	indexRegistry := parser.NewRegistry()
	codeWriter := codeindex.NewCodeWriter(db, func(path string, src []byte) (codeindex.ParseResult, error) {
		return indexRegistry.Parse(path, src), nil
	}, nil, nil)
	indexService := service.NewIndexService(codeWriter)
	indexHandler := handler.NewCodeIndexHandler(indexService)

	server := grpc.NewServer()
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

func flagDefault(flags *config.FlagSet, name string) bool {
	if flags == nil {
		return false
	}
	flag, ok := flags.Get(name)
	if !ok {
		return false
	}
	return flag.Default
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
	client, err := service.NewApprovalClient(service.ApprovalClientConfig{
		Address:      cfg.Approval.Address,
		Timeout:      time.Duration(cfg.Approval.TimeoutSeconds) * time.Second,
		PollInterval: time.Duration(cfg.Approval.PollMs) * time.Millisecond,
		Approval:     approvalCfg,
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}

func subtaskExecutor(client *harnessclient.Client, cfg config.Config) service.SubtaskExecutor {
	if client == nil {
		return nil
	}
	return service.NewAgentSubtaskExecutor(client, service.SubtaskExecutorConfig{
		EntryAgent: cfg.Agent.EntryAgent,
		Model:      cfg.Agent.StageModel,
		Timeout:    time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
	})
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
