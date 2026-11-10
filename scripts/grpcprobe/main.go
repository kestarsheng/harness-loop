package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

func main() {
	target := flag.String("target", "127.0.0.1:50008", "gRPC target")
	timeout := flag.Duration("timeout", 5*time.Second, "dial timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, *target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		fmt.Fprintf(os.Stderr, "grpcprobe: dial %s: %v\n", *target, err)
		os.Exit(1)
	}
	defer conn.Close()

	client := reflectionpb.NewServerReflectionClient(conn)
	stream, err := client.ServerReflectionInfo(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grpcprobe: reflection: %v\n", err)
		os.Exit(1)
	}
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "grpcprobe: send: %v\n", err)
		os.Exit(1)
	}
	resp, err := stream.Recv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "grpcprobe: recv: %v\n", err)
		os.Exit(1)
	}
	list := resp.GetListServicesResponse()
	if list == nil {
		fmt.Fprintln(os.Stderr, "grpcprobe: no service list in response")
		os.Exit(1)
	}
	for _, svc := range list.GetService() {
		fmt.Println(svc.GetName())
	}
}
