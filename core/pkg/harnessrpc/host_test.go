// Unit tests for the JSON-RPC host (M5.5.d.a.a).
//
// Coverage targets the test-plan contract: a single well-formed
// request round-trips through Run; malformed JSON yields a -32700
// response with null id; an unknown method yields -32601 with the
// method name in the message; a handler returning an error yields
// -32603 with err.Error() in the message. All tests drive Run over an
// io.Pipe pair so the wire shape is provable without a subprocess.

package harnessrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
)

// errorEnvelope is the test-only decoder shape for parsing the host's
// error responses. The package's wire types are unexported by design;
// tests need a local mirror.
type errorEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// runHostInBackground starts host.Run on a goroutine and returns a
// stdin writer, a function that reads the next response line, and a
// teardown that closes stdin and waits for Run to return.
func runHostInBackground(t *testing.T, host *harnessrpc.Host) (
	stdinW io.WriteCloser,
	readResponse func() string,
	teardown func() error,
) {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- host.Run(ctx, stdinR, stdoutW)
		_ = stdoutW.Close()
	}()

	readResponse = func() string {
		t.Helper()
		var sb strings.Builder
		buf := make([]byte, 1)
		for {
			n, err := stdoutR.Read(buf)
			if n == 1 {
				if buf[0] == '\n' {
					return sb.String()
				}
				sb.WriteByte(buf[0])
			}
			if err != nil {
				if err == io.EOF && sb.Len() > 0 {
					return sb.String()
				}
				t.Fatalf("readResponse: %v", err)
			}
		}
	}

	teardown = func() error {
		_ = stdinW.Close()
		cancel()
		select {
		case err := <-runDone:
			return err
		case <-time.After(2 * time.Second):
			t.Fatal("teardown: host.Run did not return within 2s")
			return nil
		}
	}

	return stdinW, readResponse, teardown
}

func writeLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		t.Fatalf("writeLine: %v", err)
	}
}

// TestHostRun_DispatchesAndResponds covers the happy-path round trip:
// a registered method receives params and the host writes a JSON-RPC
// success response with the matching id.
func TestHostRun_DispatchesAndResponds(t *testing.T) {
	host := harnessrpc.NewHost()
	host.Register("ping", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]string{"reply": "pong"}, nil
	})

	stdin, readResp, teardown := runHostInBackground(t, host)
	writeLine(t, stdin, `{"jsonrpc":"2.0","id":7,"method":"ping"}`)

	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(readResp()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.JSONRPC != "2.0" || string(resp.ID) != "7" || string(resp.Result) != `{"reply":"pong"}` {
		t.Errorf("unexpected response: %+v", resp)
	}
	if err := teardown(); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// TestHostRun_ParseError_NullID validates JSON-RPC 2.0 §5.1: a body
// that fails to parse returns id=null and code=-32700.
func TestHostRun_ParseError_NullID(t *testing.T) {
	host := harnessrpc.NewHost()
	stdin, readResp, teardown := runHostInBackground(t, host)
	writeLine(t, stdin, "not json")

	var resp errorEnvelope
	if err := json.Unmarshal([]byte(readResp()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(resp.ID) != "null" {
		t.Errorf("id = %s, want null", string(resp.ID))
	}
	if resp.Error.Code != harnessrpc.ErrCodeParseError {
		t.Errorf("code = %d, want %d", resp.Error.Code, harnessrpc.ErrCodeParseError)
	}
	if err := teardown(); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// TestHostRun_MethodNotFound validates -32601: the message embeds the
// method name verbatim and the original id round-trips.
func TestHostRun_MethodNotFound(t *testing.T) {
	host := harnessrpc.NewHost()
	stdin, readResp, teardown := runHostInBackground(t, host)
	writeLine(t, stdin, `{"jsonrpc":"2.0","id":3,"method":"missing.method"}`)

	var resp errorEnvelope
	if err := json.Unmarshal([]byte(readResp()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(resp.ID) != "3" || resp.Error.Code != harnessrpc.ErrCodeMethodNotFound {
		t.Errorf("unexpected response: %+v", resp)
	}
	if !strings.Contains(resp.Error.Message, "missing.method") {
		t.Errorf("message %q does not contain method name", resp.Error.Message)
	}
	if err := teardown(); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// TestHostRun_HandlerError validates -32603: a non-nil handler error
// is lifted into an InternalError response with err.Error() as the
// message.
func TestHostRun_HandlerError(t *testing.T) {
	host := harnessrpc.NewHost()
	host.Register("boom", func(_ context.Context, _ json.RawMessage) (any, error) {
		return nil, errors.New("kaboom")
	})

	stdin, readResp, teardown := runHostInBackground(t, host)
	writeLine(t, stdin, `{"jsonrpc":"2.0","id":4,"method":"boom"}`)

	var resp errorEnvelope
	if err := json.Unmarshal([]byte(readResp()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != harnessrpc.ErrCodeInternalError || resp.Error.Message != "kaboom" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if err := teardown(); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// TestRegister_NilHandler_Panics validates that a nil handler is
// rejected at registration time rather than during dispatch.
func TestRegister_NilHandler_Panics(t *testing.T) {
	host := harnessrpc.NewHost()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register(nil) did not panic")
		}
	}()
	host.Register("oops", nil)
}
