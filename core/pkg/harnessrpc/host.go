// Package harnessrpc hosts the Go-side stdio JSON-RPC 2.0 server that
// the TS Watchkeeper harness drives via the [RpcClient] in
// `harness/src/jsonrpc.ts`. ROADMAP §M5.5.d.a.a.
//
// Wire protocol: JSON-RPC 2.0 over newline-delimited JSON (NDJSON), one
// JSON value per line, UTF-8, LF separator — symmetric with
// `harness/src/jsonrpc.ts`. Ids are echoed verbatim; `null`-id
// responses are reserved for parse errors per §5.1.
//
// This package introduces the bidirectional seam without wiring it into
// `core/pkg/runtime` or the harness-spawn flow: tests drive [Host.Run]
// over an io.Pipe pair. M5.5.d.a.b will register the first real method
// (`notebook.remember`) on top.
//
// Concurrency: [Host.Register] runs at boot before [Host.Run]. [Host.Run]
// reads requests serially and dispatches each in its own goroutine so a
// slow handler does not block the loop; writes to `out` are serialized.
package harnessrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// RPCError is a typed error that [MethodHandler] implementations return when
// they need to emit a specific JSON-RPC error code instead of the default
// [ErrCodeInternalError] (-32603). The [Host] dispatcher checks for this type
// via [errors.As] before falling back to the generic -32603 mapping.
//
// Use [NewRPCError] to construct one; callers can match with [errors.As].
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// NewRPCError constructs an [RPCError] with the given code and message.
// Handlers that need -32602 (invalid params) call
// NewRPCError(ErrCodeInvalidParams, "...").
func NewRPCError(code int, message string) *RPCError {
	return &RPCError{Code: code, Message: message}
}

// ErrCodeInvalidParams is the JSON-RPC 2.0 error code for a request whose
// params cannot be decoded or fail application-level validation. Handlers
// return NewRPCError(ErrCodeInvalidParams, ...) to signal this condition.
const ErrCodeInvalidParams = -32602

// JSON-RPC 2.0 standard error codes the [Host] emits. Application-level
// codes belong outside the reserved `-32768..-32000` range.
const (
	// JSONRPCVersion is the literal version field every envelope carries.
	JSONRPCVersion = "2.0"

	// ErrCodeParseError is returned for malformed JSON on the wire (§5.1).
	// The response carries `id: null` because the id cannot be recovered.
	ErrCodeParseError = -32700

	// ErrCodeInvalidRequest covers a body that parses as JSON but fails
	// the JSON-RPC envelope check (wrong jsonrpc, missing/empty method,
	// non-id-shaped id). Recovered id is preserved when available.
	ErrCodeInvalidRequest = -32600

	// ErrCodeMethodNotFound signals an unknown method. The response
	// message embeds the method name verbatim for debuggability.
	ErrCodeMethodNotFound = -32601

	// ErrCodeInternalError is the generic handler-failure code; the
	// host emits this when a [MethodHandler] returns a non-nil error
	// and uses err.Error() as the message.
	ErrCodeInternalError = -32603
)

// MethodHandler is the per-method callback the [Host] dispatches into.
// `params` is the raw JSON body of the request's `params` field, or
// nil when the field was absent. A non-nil err is lifted into a
// [ErrCodeInternalError] response with err.Error() as the message.
type MethodHandler func(ctx context.Context, params json.RawMessage) (any, error)

// Host is the JSON-RPC server end of the harness ↔ core channel. The
// zero value is unusable; construct via [NewHost]. Concurrent
// [Register] is unsupported by design (registration is a boot-time
// activity).
type Host struct {
	mu      sync.Mutex
	methods map[string]MethodHandler
}

// NewHost returns a [Host] with an empty method registry.
func NewHost() *Host {
	return &Host{methods: make(map[string]MethodHandler)}
}

// Register adds `handler` under `method`. Last-call-wins on
// re-registration. Panics on a nil handler so the failure surfaces at
// boot rather than mid-dispatch.
func (h *Host) Register(method string, handler MethodHandler) {
	if handler == nil {
		panic(fmt.Sprintf("harnessrpc: nil handler for method %q", method))
	}
	h.mu.Lock()
	h.methods[method] = handler
	h.mu.Unlock()
}

// Run drives the dispatch loop: read NDJSON requests from `in`, route
// each to the registered [MethodHandler] (or to an error response when
// the line cannot be parsed / the method is unknown / the handler
// returns an error), and write the response line to `out`.
//
// Returns nil on a clean EOF (peer closed `in`) and the underlying
// scanner error otherwise. Blocks until EOF or `ctx.Done()`. Each
// request dispatches in its own goroutine; writes to `out` are
// serialized internally. The handler receives the same `ctx` so it
// can react to cancellation; the host imposes no per-request deadline.
//
// Trust: peer is the harness child process; flooding is treated as a
// peer bug, not a DOS vector. Bounded concurrency is M6 work if the
// trust assumption changes.
func (h *Host) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	const maxLine = 1 << 20
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)

	var writeMu sync.Mutex
	// writeErr stores the first write error observed by any dispatched
	// goroutine. Once set, the read loop exits so that callers whose
	// pending promises would hang forever get an early error instead.
	var writeErr atomic.Pointer[error]

	writeLine := func(line []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := out.Write(line); err != nil {
			writeErr.CompareAndSwap(nil, &err)
			return err
		}
		if _, err := out.Write([]byte{'\n'}); err != nil {
			writeErr.CompareAndSwap(nil, &err)
			return err
		}
		return nil
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if p := writeErr.Load(); p != nil {
			return fmt.Errorf("harnessrpc: write: %w", *p)
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("harnessrpc: scan: %w", err)
			}
			return nil
		}
		// scanner.Bytes() reuses its internal buffer; copy before the
		// goroutine launch.
		line := scanner.Bytes()
		buf := make([]byte, len(line))
		copy(buf, line)

		wg.Add(1)
		go func(payload []byte) {
			defer wg.Done()
			respLine, ok := h.dispatchOne(ctx, payload)
			if !ok {
				return
			}
			_ = writeLine(respLine)
		}(buf)
	}
}

// dispatchOne handles a single NDJSON line: parse, dispatch, serialize
// response. Returns (line, true) when a response should be written and
// (_, false) for blank lines (silently skipped — mirrors the harness
// reader's tolerance under cat-style smoke tests).
func (h *Host) dispatchOne(ctx context.Context, line []byte) ([]byte, bool) {
	if isBlank(line) {
		return nil, false
	}

	id, method, params, parseErr := parseRequest(line)
	if parseErr != nil {
		encoded, _ := json.Marshal(buildErrorResponse(id, parseErr.code, parseErr.message))
		return encoded, true
	}

	h.mu.Lock()
	handler, found := h.methods[method]
	h.mu.Unlock()
	if !found {
		encoded, _ := json.Marshal(buildErrorResponse(id, ErrCodeMethodNotFound,
			fmt.Sprintf("method not found: %s", method)))
		return encoded, true
	}

	result, handlerErr := handler(ctx, params)
	if handlerErr != nil {
		code := ErrCodeInternalError
		var rpcErr *RPCError
		if errors.As(handlerErr, &rpcErr) {
			code = rpcErr.Code
		}
		encoded, _ := json.Marshal(buildErrorResponse(id, code, handlerErr.Error()))
		return encoded, true
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		encoded, _ := json.Marshal(buildErrorResponse(id, ErrCodeInternalError,
			fmt.Sprintf("marshal result: %v", err)))
		return encoded, true
	}
	encoded, _ := json.Marshal(successResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Result:  resultBytes,
	})
	return encoded, true
}

// requestEnvelope mirrors the JSON-RPC 2.0 request shape. ID is
// json.RawMessage so the wire id (string, number, or null) round-trips
// verbatim.
type requestEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type successResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
}

type errorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   errorBody       `json:"error"`
}

type errorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// nullID is the canonical JSON `null` literal used when the host
// cannot recover an id from a malformed envelope.
var nullID = json.RawMessage(`null`)

func buildErrorResponse(id json.RawMessage, code int, message string) errorResponse {
	if len(id) == 0 {
		id = nullID
	}
	return errorResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error:   errorBody{Code: code, Message: message},
	}
}

type parseErr struct {
	code    int
	message string
}

// parseRequest extracts (id, method, params) from a request line. On
// failure returns (recoveredID, "", nil, err) where recoveredID is the
// verbatim wire bytes when the envelope's id slot is shape-valid and
// nullID otherwise.
func parseRequest(line []byte) (json.RawMessage, string, json.RawMessage, *parseErr) {
	var env requestEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nullID, "", nil, &parseErr{code: ErrCodeParseError, message: err.Error()}
	}

	recoveredID := env.ID
	if !isValidID(recoveredID) {
		recoveredID = nullID
	}

	if env.JSONRPC != JSONRPCVersion {
		return recoveredID, "", nil, &parseErr{
			code:    ErrCodeInvalidRequest,
			message: fmt.Sprintf("jsonrpc field must be %q", JSONRPCVersion),
		}
	}
	if env.Method == "" {
		return recoveredID, "", nil, &parseErr{
			code:    ErrCodeInvalidRequest,
			message: "method field must be a non-empty string",
		}
	}
	if !isValidID(env.ID) {
		return nullID, "", nil, &parseErr{
			code:    ErrCodeInvalidRequest,
			message: "id field must be string, number, or null",
		}
	}

	return env.ID, env.Method, env.Params, nil
}

// isValidID returns true when raw is a JSON literal JSON-RPC 2.0
// accepts as an id (string, number, or null).
func isValidID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '"':
		return true
	case 'n':
		return string(raw) == "null"
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		return json.Unmarshal(raw, &n) == nil
	default:
		return false
	}
}

func isBlank(line []byte) bool {
	for _, b := range line {
		if b != ' ' && b != '\t' && b != '\r' {
			return false
		}
	}
	return true
}

// EchoParams is the request shape the [EchoHandler] decodes. Exported
// so tests and the M5.5.d.a.b sibling can share it.
type EchoParams struct {
	Text string `json:"text"`
}

// EchoResult is the response shape the [EchoHandler] returns — the
// canonical no-op round-trip used to validate the framing end-to-end
// per AC3 (`echo({text}) → {text}`).
type EchoResult struct {
	Text string `json:"text"`
}

// EchoHandler is the canonical no-op method that round-trips `text`.
// Lives here as the wire-protocol smoke test, not a production verb.
// Returns [ErrEchoInvalidParams] when the params do not decode.
func EchoHandler(_ context.Context, params json.RawMessage) (any, error) {
	if len(params) == 0 {
		return nil, ErrEchoInvalidParams
	}
	var p EchoParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEchoInvalidParams, err)
	}
	return EchoResult(p), nil
}

// ErrEchoInvalidParams is returned by [EchoHandler] when the params
// cannot be decoded as [EchoParams]; sentinel for [errors.Is] in tests.
var ErrEchoInvalidParams = errors.New("echo: params must decode as {text: string}")
