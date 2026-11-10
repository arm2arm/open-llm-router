// Command mock-llm runs the mock backend of package mockllm as a server, for
// the Compose stack. It is configured through the environment:
//
//	MOCK_LISTEN       address to listen on (default ":8000")
//	MOCK_MODELS       served model names, comma-separated (default "chat-model")
//	MOCK_TOKENS       output tokens of a text answer (default 16)
//	MOCK_TOKEN_DELAY  pause before each output token, such as "20ms" (default none)
//	MOCK_FAIL         error, unhealthy, drop or drop-mid-stream (default none)
//	MOCK_THINKING     true to start every Anthropic answer with a thinking block
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arm2arm/open-llm-router/internal/mockllm"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("mock-llm: ")

	opts, err := mockllm.OptionsFromEnv(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("MOCK_LISTEN")
	if addr == "" {
		addr = ":8000"
	}
	srv := &http.Server{Addr: addr, Handler: mockllm.New(opts), ReadHeaderTimeout: 10 * time.Second}

	// A mock has nothing to finish: on SIGTERM or Ctrl-C it stops at once.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	log.Printf("listening on %s (token delay %s, failure mode %q)", addr, opts.TokenDelay, opts.Fail)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
