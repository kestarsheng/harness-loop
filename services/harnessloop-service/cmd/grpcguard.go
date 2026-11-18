package main

import (
	"context"
	"log"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The gRPC server had no interceptor at all (T-P2-35), so a panic inside any
// RPC handler killed the process with exit 2 and left the caller with a broken
// connection instead of an error. With orchestrator on, one malformed DAG or a
// nil map in the plan/scheduler/aggregation path is enough to reach that state.
//
// These interceptors keep the blast radius at one request: the panic is logged
// with its stack and the caller gets codes.Internal.
func recoveryUnaryInterceptor(logger *log.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			logPanic(logger, "unary", info.FullMethod, rec)
			resp, err = nil, status.Error(codes.Internal, "internal error (panic recovered)")
		}()
		return handler(ctx, req)
	}
}

func recoveryStreamInterceptor(logger *log.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			logPanic(logger, "stream", info.FullMethod, rec)
			err = status.Error(codes.Internal, "internal error (panic recovered)")
		}()
		return handler(srv, ss)
	}
}

func logPanic(logger *log.Logger, kind, method string, rec any) {
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("harnessloop: recovered panic in %s handler %s (process kept alive): %v\n%s",
		kind, method, rec, debug.Stack())
}
