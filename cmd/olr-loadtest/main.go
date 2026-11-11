// Command olr-loadtest measures streaming overhead, cancellations and idle state.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arm2arm/open-llm-router/internal/loadtest"
)

func main() {
	var o loadtest.Options
	var direct, weights string
	flag.StringVar(&o.Router, "router", "http://localhost:8080", "router origin")
	flag.StringVar(&direct, "direct", "http://localhost:8001,http://localhost:8002", "comma-separated direct backend origins (never the router)")
	flag.StringVar(&weights, "weights", "2,1", "comma-separated direct backend weights")
	flag.StringVar(&o.Model, "model", "chat-model", "router model")
	flag.StringVar(&o.DirectModel, "direct-model", "", "backend model, when different from the router alias")
	flag.IntVar(&o.Concurrency, "concurrency", 256, "simultaneous streaming clients, per protocol")
	flag.IntVar(&o.Requests, "requests", 0, "requests per measured batch (default: twice concurrency)")
	flag.IntVar(&o.Runs, "runs", 3, "repetitions, with idle leak snapshots after each")
	flag.IntVar(&o.Tokens, "tokens", 128, "maximum output tokens (exact in mock mode)")
	flag.Float64Var(&o.AbortFraction, "abort-fraction", .1, "share of clients intentionally disconnected in a separate batch")
	flag.IntVar(&o.AbortAfter, "abort-after", 4, "content events before intentional disconnect")
	flag.DurationVar(&o.Timeout, "timeout", 60*time.Second, "deadline per request")
	flag.DurationVar(&o.Settle, "settle", 2*time.Second, "quiet time before each idle runtime snapshot")
	flag.BoolVar(&o.Mock, "mock", false, "enable mock headers and backend cancellation checks")
	flag.StringVar(&o.MockDelay, "mock-delay", "", "override both mocks' per-token delay; 0s for saturation tests")
	flag.BoolVar(&o.CheckPerformance, "check-performance", true, "fail on performance targets (disable for noisy CI or saturation exploration)")
	flag.Float64Var(&o.MaxAddedP95MS, "max-added-p95-ms", 5, "maximum difference of router/direct p95 time to first content token")
	flag.Float64Var(&o.MaxTPSLoss, "max-tps-loss", .02, "maximum relative per-stream and aggregate throughput loss")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: olr-loadtest [flags]\n\nRequired: OLR_API_KEY, OLR_METRICS_TOKEN. Optional: OLR_DIRECT_API_KEY,\nOLR_LOADTEST_DATABASE_URL (read-only usage verification). Secrets are never flags.\nThe command sends synthetic prompts and writes a JSON report to stdout.")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	o.APIKey, o.DirectKey = os.Getenv("OLR_API_KEY"), os.Getenv("OLR_DIRECT_API_KEY")
	o.MetricsToken, o.DatabaseURL = os.Getenv("OLR_METRICS_TOKEN"), os.Getenv("OLR_LOADTEST_DATABASE_URL")
	o.Direct = strings.Split(direct, ",")
	for _, value := range strings.Split(weights, ",") {
		weight, err := strconv.ParseFloat(value, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "weights must be numbers")
			os.Exit(2)
		}
		o.Weights = append(o.Weights, weight)
	}
	if o.Requests == 0 {
		o.Requests = 2 * o.Concurrency
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := loadtest.Run(ctx, o)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(report); encodeErr != nil {
		fmt.Fprintln(os.Stderr, "write load-test report failed")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "olr-loadtest:", err)
		os.Exit(1)
	}
	if !report.Passed {
		os.Exit(1)
	}
}
