package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      string `json:"id"`
}

type rpcResponseEnvelope struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type rpcNotification struct {
	Method string
	Params json.RawMessage
}

type rpcResponse struct {
	Result json.RawMessage
	Error  *rpcError
	Err    error
}

type jsonRPCClient struct {
	conn net.Conn
	enc  *json.Encoder

	writeMu sync.Mutex

	nextID atomic.Uint64

	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse

	notifications chan rpcNotification
	errCh         chan error

	closeOnce sync.Once
	done      chan struct{}
}

func newJSONRPCClient(conn net.Conn) *jsonRPCClient {
	c := &jsonRPCClient{
		conn:          conn,
		enc:           json.NewEncoder(conn),
		pending:       make(map[string]chan rpcResponse),
		notifications: make(chan rpcNotification, 128),
		errCh:         make(chan error, 1),
		done:          make(chan struct{}),
	}

	go c.readLoop()

	return c
}

func (c *jsonRPCClient) Notifications() <-chan rpcNotification {
	return c.notifications
}

func (c *jsonRPCClient) Errors() <-chan error {
	return c.errCh
}

func (c *jsonRPCClient) Close() {
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
		<-c.done
	})
}

//nolint:cyclop // response handling branches on context, stream state, and rpc errors.
func (c *jsonRPCClient) Call(ctx context.Context, method string, params any, out any) error {
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	respCh := make(chan rpcResponse, 1)

	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}

	c.writeMu.Lock()
	err := c.enc.Encode(req)
	c.writeMu.Unlock()

	if err != nil {
		c.removePending(id)

		return fmt.Errorf("sending json-rpc request: %w", err)
	}

	select {
	case <-ctx.Done():
		c.removePending(id)

		return fmt.Errorf("waiting for json-rpc response: %w", ctx.Err())
	case <-c.done:
		return errors.New("json-rpc connection closed")
	case resp := <-respCh:
		if resp.Err != nil {
			return resp.Err
		}

		if resp.Error != nil {
			return fmt.Errorf("json-rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}

		if out != nil && len(resp.Result) > 0 && string(resp.Result) != "null" {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("decoding json-rpc response: %w", err)
			}
		}

		return nil
	}
}

//nolint:cyclop // stream parser distinguishes notifications, responses, and malformed frames.
func (c *jsonRPCClient) readLoop() {
	defer close(c.done)

	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg rpcResponseEnvelope
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			slog.Debug("signal: skipping malformed json-rpc line", "error", err)

			continue
		}

		if msg.Method != "" && len(msg.ID) == 0 {
			c.notifications <- rpcNotification{Method: msg.Method, Params: msg.Params}

			continue
		}

		id := normalizeJSONRPCID(msg.ID)
		if id == "" {
			continue
		}

		c.pendingMu.Lock()
		respCh := c.pending[id]
		delete(c.pending, id)
		c.pendingMu.Unlock()

		if respCh != nil {
			respCh <- rpcResponse{Result: msg.Result, Error: msg.Error}
		}
	}

	readErr := scanner.Err()
	if readErr == nil {
		readErr = io.EOF
	}

	c.failPending(fmt.Errorf("json-rpc read loop ended: %w", readErr))

	select {
	case c.errCh <- readErr:
	default:
	}
}

func (c *jsonRPCClient) removePending(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *jsonRPCClient) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	for id, ch := range c.pending {
		ch <- rpcResponse{Err: err}

		delete(c.pending, id)
	}
}

func normalizeJSONRPCID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}

	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		return num.String()
	}

	return ""
}
