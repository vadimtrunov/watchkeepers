// Integration tests for the harness ↔ Go JSON-RPC bridge (M5.5.d.a.a).
//
// Wires the Go [Host] (with EchoHandler registered) to a small in-test
// harness-impersonator that hand-crafts the same NDJSON shapes the TS
// RpcClient emits. The two halves communicate over an io.Pipe pair —
// no Node, no subprocess. Validates AC3 (echo round-trip) and AC5
// (concurrent dispatch correlation) — the latter under `go test -race`.

package harnessrpc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
)

type pendingMap struct {
	mu      sync.Mutex
	entries map[int64]chan responseFrame
}

type responseFrame struct {
	result json.RawMessage
	err    *responseError
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func newPendingMap() *pendingMap {
	return &pendingMap{entries: make(map[int64]chan responseFrame)}
}

func (p *pendingMap) put(id int64) chan responseFrame {
	ch := make(chan responseFrame, 1)
	p.mu.Lock()
	p.entries[id] = ch
	p.mu.Unlock()
	return ch
}

func (p *pendingMap) take(id int64) (chan responseFrame, bool) {
	p.mu.Lock()
	ch, ok := p.entries[id]
	if ok {
		delete(p.entries, id)
	}
	p.mu.Unlock()
	return ch, ok
}

func (p *pendingMap) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// rpcShim is the test-side counterpart of the TS RpcClient: numeric id,
// pending-request map, response-reader goroutine.
type rpcShim struct {
	w        io.Writer
	nextID   int64
	pending  *pendingMap
	readDone chan error
}

func newRPCShim(w io.Writer, r io.Reader) *rpcShim {
	s := &rpcShim{
		w:        w,
		pending:  newPendingMap(),
		readDone: make(chan error, 1),
	}
	go s.readLoop(r)
	return s
}

func (s *rpcShim) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var env struct {
			ID     json.Number     `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *responseError  `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			s.readDone <- fmt.Errorf("decode response line: %w", err)
			return
		}
		id, err := env.ID.Int64()
		if err != nil {
			continue
		}
		ch, ok := s.pending.take(id)
		if !ok {
			continue
		}
		ch <- responseFrame{result: env.Result, err: env.Error}
		close(ch)
	}
	s.readDone <- scanner.Err()
}

// request emits a JSON-RPC request and blocks for the matching response.
func (s *rpcShim) request(method string, params any) (json.RawMessage, *responseError, error) {
	id := atomic.AddInt64(&s.nextID, 1)
	ch := s.pending.put(id)

	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		envelope["params"] = params
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.w.Write(line); err != nil {
		return nil, nil, err
	}
	if _, err := s.w.Write([]byte{'\n'}); err != nil {
		return nil, nil, err
	}

	select {
	case frame, ok := <-ch:
		if !ok {
			return nil, nil, fmt.Errorf("response channel closed without delivery")
		}
		return frame.result, frame.err, nil
	case <-time.After(5 * time.Second):
		return nil, nil, fmt.Errorf("request %s timed out", method)
	}
}

// startBridge wires a [harnessrpc.Host] to a test-side rpcShim through
// a pair of io.Pipes; returns the shim and a teardown.
func startBridge(t *testing.T, host *harnessrpc.Host) (*rpcShim, func()) {
	t.Helper()

	hostStdinR, hostStdinW := io.Pipe()
	hostStdoutR, hostStdoutW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- host.Run(ctx, hostStdinR, hostStdoutW)
		_ = hostStdoutW.Close()
	}()

	shim := newRPCShim(hostStdinW, hostStdoutR)

	teardown := func() {
		_ = hostStdinW.Close()
		cancel()
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Fatal("teardown: host.Run did not return within 2s")
		}
		select {
		case <-shim.readDone:
		case <-time.After(2 * time.Second):
			t.Fatal("teardown: shim read loop did not finish within 2s")
		}
	}
	return shim, teardown
}

// TestIntegration_Echo_RoundTrip validates AC3 — the shim emits an
// `echo` request and the host responds with the same text.
func TestIntegration_Echo_RoundTrip(t *testing.T) {
	host := harnessrpc.NewHost()
	host.Register("echo", harnessrpc.EchoHandler)

	shim, teardown := startBridge(t, host)
	defer teardown()

	result, errEnv, err := shim.request("echo", map[string]string{"text": "hello"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if errEnv != nil {
		t.Fatalf("expected ok response, got error %+v", errEnv)
	}
	var got harnessrpc.EchoResult
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.Text != "hello" {
		t.Errorf("got.Text = %q, want %q", got.Text, "hello")
	}
}

// TestIntegration_ConcurrentRequests_Correlation validates AC5: eight
// concurrent requests are correctly correlated by id. Runs under
// `-race` so concurrent dispatch correctness is also covered.
func TestIntegration_ConcurrentRequests_Correlation(t *testing.T) {
	host := harnessrpc.NewHost()
	host.Register("echo", harnessrpc.EchoHandler)

	shim, teardown := startBridge(t, host)
	defer teardown()

	var wg sync.WaitGroup
	type outcome struct {
		text string
		err  error
	}
	got := make([]outcome, 8)
	for i := 0; i < len(got); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			text := fmt.Sprintf("msg-%d", idx)
			res, errEnv, err := shim.request("echo", map[string]string{"text": text})
			if err != nil {
				got[idx] = outcome{err: err}
				return
			}
			if errEnv != nil {
				got[idx] = outcome{err: fmt.Errorf("err response: %+v", errEnv)}
				return
			}
			var r harnessrpc.EchoResult
			if err := json.Unmarshal(res, &r); err != nil {
				got[idx] = outcome{err: err}
				return
			}
			got[idx] = outcome{text: r.Text}
		}(i)
	}
	wg.Wait()

	for i, o := range got {
		if o.err != nil {
			t.Errorf("call %d: %v", i, o.err)
			continue
		}
		want := fmt.Sprintf("msg-%d", i)
		if o.text != want {
			t.Errorf("call %d: got %q, want %q", i, o.text, want)
		}
	}
	if shim.pending.size() != 0 {
		t.Errorf("pending map size = %d, want 0 (memory-leak guard)", shim.pending.size())
	}
}
