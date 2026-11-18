package main

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRecoveryUnaryInterceptorCatchesPanic(t *testing.T) {
	quiet := log.New(io.Discard, "", 0)
	interceptor := recoveryUnaryInterceptor(quiet)
	info := &grpc.UnaryServerInfo{FullMethod: "/harnessloop.HarnessLoopService/SubmitTask"}

	_, err := interceptor(context.Background(), nil, info, func(ctx context.Context, req any) (any, error) {
		panic("nil map write in the orchestrator bridge")
	})
	if err == nil {
		t.Fatal("a panicking handler must return an error")
	}
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code = %s, want Internal (the caller must not see a dead connection)", got)
	}
	if !strings.Contains(err.Error(), "panic recovered") {
		t.Fatalf("err = %v, want an explainable message", err)
	}
}

func TestRecoveryUnaryInterceptorPassesThrough(t *testing.T) {
	interceptor := recoveryUnaryInterceptor(log.New(io.Discard, "", 0))
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/Ok"}
	resp, err := interceptor(context.Background(), "req", info, func(ctx context.Context, req any) (any, error) {
		return "resp", nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp != "resp" {
		t.Fatalf("resp = %v", resp)
	}

	want := errors.New("business failure")
	if _, err := interceptor(context.Background(), nil, info, func(ctx context.Context, req any) (any, error) {
		return nil, want
	}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the handler error untouched", err)
	}
}

func TestRecoveryStreamInterceptorCatchesPanic(t *testing.T) {
	interceptor := recoveryStreamInterceptor(log.New(io.Discard, "", 0))
	info := &grpc.StreamServerInfo{FullMethod: "/svc/Stream"}
	err := interceptor(nil, nil, info, func(srv any, ss grpc.ServerStream) error {
		panic("stream handler exploded")
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %s, want Internal", status.Code(err))
	}
}

// logPanic is called from the recover path; it must not itself panic when the
// caller forgot to pass a logger.
func TestLogPanicToleratesNilLogger(t *testing.T) {
	logPanic(nil, "unary", "/svc/X", "boom")
}
