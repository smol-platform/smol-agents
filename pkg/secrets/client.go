package secrets

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client is the agent-side entry point to the broker.
type Client struct {
	SocketPath string
	DialFunc   func(ctx context.Context) (net.Conn, error) // overrides for tests

	mu   sync.Mutex
	conn net.Conn
}

// NewClient returns a Client that will lazily dial socketPath.
func NewClient(socketPath string) *Client {
	return &Client{SocketPath: socketPath}
}

// Lease asks the broker for a lease on name with the requested TTL. If
// ttl <= 0, the broker uses its default.
func (c *Client) Lease(ctx context.Context, name string, ttl time.Duration) (*Lease, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidRequest)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(conn, request{Kind: reqLease, Name: name, TTL: ttl}); err != nil {
		c.dropConn()
		return nil, fmt.Errorf("secrets: write request: %w", err)
	}
	var resp response
	if err := readFrame(conn, &resp); err != nil {
		c.dropConn()
		return nil, fmt.Errorf("secrets: read response: %w", err)
	}
	if resp.ErrorCode != "" {
		return nil, errorFromCode(resp.ErrorCode, resp.ErrorMessage)
	}
	if resp.Lease == nil {
		return nil, errors.New("secrets: empty response with no error")
	}
	return resp.Lease, nil
}

// Close closes the underlying connection if any.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	if c.DialFunc != nil {
		conn, err := c.DialFunc(ctx)
		if err != nil {
			return nil, err
		}
		c.conn = conn
		return conn, nil
	}
	d := &net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("secrets: dial %s: %w", c.SocketPath, err)
	}
	c.conn = conn
	return conn, nil
}

func (c *Client) dropConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}
