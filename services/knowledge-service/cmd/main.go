package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agent-platform/pkg/config"
	"agent-platform/pkg/llm"
	"agent-platform/pkg/mongodb"
	pb "agent-platform/pkg/pb/knowledge"
	"agent-platform/pkg/qdrant"
	"agent-platform/services/knowledge-service/internal/handler"
	"agent-platform/services/knowledge-service/internal/indexing"
	"agent-platform/services/knowledge-service/internal/repository"
	"agent-platform/services/knowledge-service/internal/rerank"
	"agent-platform/services/knowledge-service/internal/service"

	"google.golang.org/grpc"
)

func main() {
	// Load config
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "./config.yaml"
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Printf("Using default config: %v", err)
		cfg = config.LoadDefault()
	}

	// Create LLM client for embeddings
	llmClient, err := llm.NewClient(llm.Config{
		Provider:       cfg.LLM.Provider,
		APIKey:         cfg.LLM.APIKey,
		BaseURL:        cfg.LLM.BaseURL,
		Model:          cfg.LLM.Model,
		EmbeddingModel: cfg.LLM.EmbeddingModel,
	})
	if err != nil {
		log.Fatalf("Failed to create LLM client: %v", err)
	}

	// Create Qdrant client (optional)
	var qdrantClient *qdrant.Client
	if cfg.Qdrant.URL != "" && cfg.Qdrant.URL != "http://localhost:6333" {
		qdrantClient = qdrant.NewClient(qdrant.Config{
			URL:        cfg.Qdrant.URL,
			Collection: cfg.Qdrant.Collection,
		})

		// Ensure the collection exists (text-embedding-v3 has 1024 dimensions).
		// Without this the first upload fails with a Qdrant 404 - the collection
		// is never auto-created. Mirrors memory-service's startup init.
		ctx := context.Background()
		exists, err := qdrantClient.CollectionExists(ctx)
		if err != nil {
			log.Printf("Warning: Failed to check collection existence: %v", err)
		} else if !exists {
			if err := qdrantClient.CreateCollection(ctx, 1024); err != nil {
				log.Printf("Warning: Failed to create collection: %v", err)
			} else {
				log.Printf("Created Qdrant collection: %s", cfg.Qdrant.Collection)
			}
		}
		log.Printf("Qdrant configured: %s", cfg.Qdrant.URL)
	} else {
		log.Println("Qdrant not configured, using in-memory storage")
	}

	// Create MongoDB client (optional)
	var mongoClient *mongodb.Client
	if cfg.MongoDB.URL != "" && cfg.MongoDB.URL != "mongodb://localhost:27017" {
		mongoClient, err = mongodb.NewClient(context.Background(), mongodb.Config{
			URI:      cfg.MongoDB.URL,
			Database: cfg.MongoDB.Database,
		})
		if err != nil {
			log.Printf("Warning: Failed to create MongoDB client: %v", err)
			log.Println("Continuing without MongoDB - knowledge storage will be limited")
			mongoClient = nil
		}
	} else {
		log.Println("MongoDB not configured, using SQLite for document storage")
	}

	// Create document repository
	docRepo := repository.NewDocumentRepository(mongoClient, qdrantClient)

	// Create knowledge service
	knowledgeService := service.NewKnowledgeService(llmClient, docRepo, cfg)
	if r, desc := newReranker(llmClient, cfg); r != nil {
		knowledgeService.SetReranker(r)
		log.Printf("knowledge rerank enabled (%s)", desc)
	} else {
		log.Printf("knowledge rerank disabled, fusion order will be used")
	}

	// 异步索引 worker：上传即 pending，后台解析/向量化后置 ready。
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	worker := startIndexWorker(rootCtx, knowledgeService, docRepo)
	if worker != nil {
		knowledgeService.SetIndexWorker(worker)
		log.Printf("knowledge index worker started (%+v)", worker.Config())
	} else {
		log.Printf("knowledge index worker disabled, upload runs synchronously")
	}

	// Create gRPC server
	server := grpc.NewServer()
	h := handler.NewKnowledgeHandler(knowledgeService)
	pb.RegisterKnowledgeServiceServer(server, h)

	// Start server
	addr := fmt.Sprintf(":%d", cfg.Server.GRPCPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	log.Printf("Knowledge Service starting on %s", addr)

	go func() {
		if err := server.Serve(listener); err != nil {
			log.Fatalf("Failed to serve: %v", err)
		}
	}()

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down Knowledge Service...")
	if worker != nil {
		worker.Stop(context.Background())
	}
	rootCancel()
	server.GracefulStop()

	// Close connections
	if mongoClient != nil {
		mongoClient.Close(context.Background())
	}

	log.Println("Knowledge Service stopped")
}

// newReranker 按环境变量选择 reranker：
//   - llm（默认）：LLM 打分，超时 3s；
//   - http：外部 cross-encoder（KNOWLEDGE_RERANK_HTTP_URL，超时独立预算）；
//   - none：关闭，直接返回融合顺序。
//
// 无论哪种实现，失败都由 service 层降级并标 rerank=degraded。
func newReranker(llmClient llm.Client, cfg *config.Config) (rerank.Reranker, string) {
	if os.Getenv("KNOWLEDGE_RERANK_ENABLED") == "false" {
		return nil, ""
	}
	url := strings.TrimSpace(os.Getenv("KNOWLEDGE_RERANK_HTTP_URL"))
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("KNOWLEDGE_RERANK_MODE")))
	if mode == "" {
		if url != "" {
			mode = "http"
		} else {
			mode = "llm"
		}
	}
	switch mode {
	case "none", "off", "disabled":
		return nil, ""
	case "http":
		if url == "" {
			log.Printf("Warning: KNOWLEDGE_RERANK_MODE=http without KNOWLEDGE_RERANK_HTTP_URL, rerank disabled")
			return nil, ""
		}
		timeout := rerank.DefaultRerankTimeout
		if raw := os.Getenv("KNOWLEDGE_RERANK_TIMEOUT_MS"); raw != "" {
			if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
		}
		client := rerank.NewHTTPReranker(url, timeout)
		client.Model = os.Getenv("KNOWLEDGE_RERANK_HTTP_MODEL")
		client.APIKey = os.Getenv("KNOWLEDGE_RERANK_HTTP_API_KEY")
		return client, fmt.Sprintf("mode=http url=%s timeout=%s top20->top5 fallback=degraded", url, timeout)
	}
	return rerank.NewLLMReranker(llmClient, cfg.LLM.Model),
		fmt.Sprintf("mode=llm model=%s timeout=3s fallback=degraded", cfg.LLM.Model)
}

// startIndexWorker 装配异步索引 worker。KNOWLEDGE_UPLOAD_MODE=sync 时退回同步上传。
func startIndexWorker(ctx context.Context, svc *service.KnowledgeService, store *repository.DocumentRepository) *indexing.Worker {
	if strings.ToLower(strings.TrimSpace(os.Getenv("KNOWLEDGE_UPLOAD_MODE"))) == "sync" {
		return nil
	}
	cfg := indexing.DefaultConfig()
	if raw := os.Getenv("KNOWLEDGE_INDEX_WORKERS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			cfg.Concurrency = n
		}
	}
	if raw := os.Getenv("KNOWLEDGE_INDEX_RETRIES"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			cfg.MaxRetries = n
		}
	}
	if raw := os.Getenv("KNOWLEDGE_INDEX_QUEUE"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			cfg.QueueSize = n
		}
	}
	worker := indexing.New(cfg, store, svc)
	worker.Start(ctx)
	return worker
}
