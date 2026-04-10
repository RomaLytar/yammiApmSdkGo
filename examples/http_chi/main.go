// http_chi shows the minimum viable wiring for a chi-based service.
//
// Run it locally:
//
//	APM_ENDPOINT=http://localhost:8890/ingest \
//	APM_TOKEN=your-token \
//	APM_SERVICE_NAME=demo \
//	APM_DEBUG=true \
//	go run ./examples/http_chi
//
// Then curl the routes and watch the apm logs:
//
//	curl http://localhost:8081/
//	curl http://localhost:8081/slow
//	curl -i http://localhost:8081/error
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	apm "github.com/RomaLytar/yammiApmSdkGo"
	apmmw "github.com/RomaLytar/yammiApmSdkGo/middleware"
)

func main() {
	cfg := apm.LoadConfigFromEnv()
	if cfg.ServiceName == "default" {
		cfg.ServiceName = "demo-http"
	}

	a, err := apm.New(cfg)
	if err != nil {
		log.Fatalf("apm.New: %v", err)
	}
	defer a.Shutdown(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		fmt.Fprintln(w, "slow done")
	})
	mux.HandleFunc("/error", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	})

	// Wrap with the APM middleware. The standard library mux is shown
	// here for clarity; chi.Mux works the same way (mux.Use(apmmw.HTTP(a))).
	handler := apmmw.HTTP(a)(mux)

	srv := &http.Server{
		Addr:    ":8081",
		Handler: handler,
	}

	go func() {
		log.Println("listening on :8081")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Graceful shutdown so the buffer drains.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	a.Shutdown(ctx)
}
