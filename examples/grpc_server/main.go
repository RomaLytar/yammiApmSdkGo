// grpc_server is a copy-paste sketch for wiring the SDK into a yammi-style
// gRPC service. It deliberately stops short of registering a real proto
// service so the example has zero external code-gen dependencies — drop
// the interceptors into your existing grpc.NewServer call and you're done.
//
//	APM_ENDPOINT=http://localhost:8890/ingest \
//	APM_TOKEN=your-token \
//	APM_SERVICE_NAME=board \
//	APM_PROMETHEUS_ENABLED=true \
//	APM_PROMETHEUS_LOCAL_URL=http://localhost:2112/metrics \
//	APM_PROMETHEUS_CUSTOM_METRICS=board_grpc_,board_events_ \
//	APM_DEBUG=true \
//	go run ./examples/grpc_server
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	apm "github.com/RomaLytar/yammiApmSdkGo"
	apmmw "github.com/RomaLytar/yammiApmSdkGo/middleware"
)

func main() {
	cfg := apm.LoadConfigFromEnv()
	if cfg.ServiceName == "default" {
		cfg.ServiceName = "demo-grpc"
	}

	a, err := apm.New(cfg)
	if err != nil {
		log.Fatalf("apm.New: %v", err)
	}
	defer a.Shutdown(context.Background())

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(apmmw.GRPCUnary(a)),
		grpc.StreamInterceptor(apmmw.GRPCStream(a)),
	)

	// Register your real services here:
	// boardpb.RegisterBoardServiceServer(srv, &boardServer{...})

	lis, err := net.Listen("tcp", ":50053")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	go func() {
		log.Println("gRPC listening on :50053")
		if err := srv.Serve(lis); err != nil {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	srv.GracefulStop()
	a.Shutdown(context.Background())
}
