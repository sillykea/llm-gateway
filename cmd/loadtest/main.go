// Command loadtest measures what the gateway adds on top of the model.
//
// It starts a mock OpenAI-compatible model server that answers after a fixed
// delay, measures that server directly, then sends the same traffic through
// the gateway (configured to use the mock, see configs/loadtest.json). The
// difference is the gateway's overhead: auth, rate limiting, the personal-data
// scan, routing, the cache and proxying.
//
//	loadtest -gateway http://localhost:8080 -key $GATEWAY_API_KEY -rate 500 -duration 30s
//
// Load is open-loop: requests start on a fixed schedule whether or not earlier
// ones have finished, and latency is measured from when each request was due.
// A closed loop (each worker waits for its last answer) slows down with the
// system under test and hides queueing, so its percentiles look better than
// what users see.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	gateway := flag.String("gateway", "http://localhost:8080", "gateway URL")
	key := flag.String("key", os.Getenv("GATEWAY_API_KEY"), "gateway API key")
	model := flag.String("model", "chat", "model alias")
	rate := flag.Int("rate", 500, "requests per second")
	duration := flag.Duration("duration", 20*time.Second, "how long to send")
	mockAddr := flag.String("mock", "127.0.0.1:9099", "where to run the mock model server")
	latency := flag.Duration("latency", 50*time.Millisecond, "mock model response time")
	repeat := flag.Float64("repeat", 0, "share of requests repeating an earlier prompt (cache hits)")
	maxP99 := flag.Duration("max-overhead-p99", 0, "fail if the gateway adds more than this at p99 (0 = report only)")
	flag.Parse()

	mock, err := startMock(*mockAddr, *latency)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer mock.Close()

	direct := run("http://"+*mockAddr+"/v1/chat/completions", "", *model, *rate, *duration/2, 0)
	viaGW := run(*gateway+"/v1/chat/completions", *key, *model, *rate, *duration, *repeat)

	fmt.Printf("open-loop at %d req/s, mock model answering in %s\n\n", *rate, *latency)
	fmt.Println("| Path | Requests | Errors | Achieved req/s | p50 | p90 | p99 | max |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	direct.row("model directly")
	viaGW.row("through the gateway")
	over := func(q float64) time.Duration { return viaGW.q(q) - direct.q(q) }
	fmt.Printf("\nGateway overhead: p50 %s, p90 %s, p99 %s\n", ms(over(0.5)), ms(over(0.9)), ms(over(0.99)))

	switch {
	case viaGW.errors > viaGW.count/1000:
		fmt.Fprintf(os.Stderr, "FAIL: %d errors (%v)\n", viaGW.errors, viaGW.firstErr)
		os.Exit(1)
	case *maxP99 > 0 && over(0.99) > *maxP99:
		fmt.Fprintf(os.Stderr, "FAIL: p99 overhead %s is over %s\n", ms(over(0.99)), *maxP99)
		os.Exit(1)
	}
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

// startMock answers chat completions after a fixed delay, like a model
// server with steady latency. Every prompt gets the same small answer.
func startMock(addr string, latency time.Duration) (*http.Server, error) {
	body, _ := json.Marshal(map[string]any{
		"id": "mock", "object": "chat.completion", "model": "mock",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
			"message": map[string]string{"role": "assistant", "content": "A short answer from the mock model."}}},
		"usage": map[string]int{"prompt_tokens": 30, "completion_tokens": 8, "total_tokens": 38},
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mock model server: %w", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		time.Sleep(latency)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	})}
	go srv.Serve(ln) //nolint:errcheck
	return srv, nil
}

type result struct {
	lat      []time.Duration
	count    int
	errors   int
	firstErr error
	elapsed  time.Duration
}

func (r *result) q(p float64) time.Duration {
	if len(r.lat) == 0 {
		return 0
	}
	return r.lat[min(len(r.lat)-1, int(p*float64(len(r.lat))))]
}

func (r *result) row(name string) {
	fmt.Printf("| %s | %d | %d | %.0f | %s | %s | %s | %s |\n", name, r.count, r.errors,
		float64(r.count)/r.elapsed.Seconds(), ms(r.q(0.5)), ms(r.q(0.9)), ms(r.q(0.99)), ms(r.lat[len(r.lat)-1]))
}

func run(url, key, model string, rate int, d time.Duration, repeat float64) *result {
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		MaxIdleConns: 4096, MaxIdleConnsPerHost: 4096, IdleConnTimeout: 30 * time.Second}}
	total := int(float64(rate) * d.Seconds())
	interval := time.Second / time.Duration(rate)
	lat := make([]time.Duration, total)
	var errs atomic.Int64
	var firstErr atomic.Value
	var wg sync.WaitGroup
	start := time.Now()
	for i := range total {
		due := start.Add(time.Duration(i) * interval)
		if wait := time.Until(due); wait > 0 {
			time.Sleep(wait)
		}
		prompt := fmt.Sprintf("load test question %d", i)
		if repeat > 0 && float64(i%1000)/1000 < repeat {
			prompt = fmt.Sprintf("load test question %d", i%50) // one of 50 popular questions
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := send(client, url, key, model, prompt)
			lat[i] = time.Since(due) // from when it was due, not when it went out
			if err != nil {
				if errs.Add(1) == 1 {
					firstErr.Store(err)
				}
			}
		}()
	}
	wg.Wait()
	r := &result{lat: lat, count: total, errors: int(errs.Load()), elapsed: time.Since(start)}
	if e, ok := firstErr.Load().(error); ok {
		r.firstErr = e
	}
	slices.Sort(r.lat)
	return r
}

func send(c *http.Client, url, key, model, prompt string) error {
	body, _ := json.Marshal(map[string]any{"model": model,
		"messages": []map[string]string{{"role": "user", "content": prompt}}})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
