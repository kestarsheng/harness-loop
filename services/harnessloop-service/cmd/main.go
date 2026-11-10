package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"agent-platform/pkg/harnessclient"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/modelregistry"
	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/pkg/harnessloop/policy/sandbox"
	"agent-platform/pkg/harnessloop/retry"
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

	runtime := service.NewRuntime(taskRepo, attemptRepo, stages)
	runtime.Events = publisher
	runtime.Policy = retryPolicy
	log.Printf("harnessloop: acceptance verifier is program-only in P0 (no model verifier wired yet)")

	h := handler.New(handler.Deps{
		Config:      cfg,
		Tasks:       taskRepo,
		Attempts:    attemptRepo,
		Events:      eventRepo,
		Checkpoints: checkpointRepo,
		Acceptances: acceptanceRepo,
		Runtime:     runtime,
		Policy:      policyEngine,
		Models:      models,
		Publisher:   publisher,
	})

	lis, err := net.Listen("tcp", cfg.ListenAddress())
	if err != nil {
		log.Fatalf("harnessloop: listen %s: %v", cfg.ListenAddress(), err)
	}

	server := grpc.NewServer()
	harnesspb.RegisterHarnessLoopServiceServer(server, h)
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
