// Package hostbridge connects container clients to the host guard. Device IDs
// and stored PIDs always belong to the host; the job and its supervisor remain
// in their original namespaces.
package hostbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
)

const DefaultSocket = "/run/canhazgpu/host.sock"

type Identity struct {
	User            string `json:"user"`
	PID             int    `json:"pid"`
	ContainerID     string `json:"container_id,omitempty"`
	Admin           bool   `json:"admin"`
	RedisDB         int    `json:"redis_db"`
	MemoryThreshold int    `json:"memory_threshold"`
}

type Request struct {
	Operation string `json:"operation"`
	TaskID    string `json:"task_id,omitempty"`
	Force     bool   `json:"force,omitempty"`
}

type response struct {
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

type Client struct{ Socket string }

func (c Client) connect(ctx context.Context, req Request) (net.Conn, *bufio.Reader, response, error) {
	var reply response
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, nil, reply, fmt.Errorf("connect to host guard at %s: %w", c.Socket, err)
	}
	deadline := time.Now().Add(45 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	reader := bufio.NewReader(conn)
	err = json.NewEncoder(conn).Encode(req)
	if err == nil {
		var line []byte
		line, err = reader.ReadBytes('\n')
		if err == nil {
			err = json.Unmarshal(line, &reply)
		}
	}
	if err == nil && reply.Error != "" {
		err = fmt.Errorf("host guard: %s", reply.Error)
	}
	if err != nil {
		_ = conn.Close()
		return nil, nil, reply, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reader, reply, nil
}

func (c Client) Call(ctx context.Context, req Request, result any) error {
	conn, _, reply, err := c.connect(ctx, req)
	if err != nil {
		return err
	}
	defer conn.Close()
	return json.Unmarshal(reply.Result, result)
}

func (c Client) Identity(ctx context.Context) (Identity, error) {
	var identity Identity
	err := c.Call(ctx, Request{Operation: "identity"}, &identity)
	return identity, err
}

func (c Client) Usage(ctx context.Context) (map[int]*types.GPUUsage, error) {
	var usage map[int]*types.GPUUsage
	err := c.Call(ctx, Request{Operation: "usage"}, &usage)
	return usage, err
}

// RedisConn tunnels the existing Redis protocol so bridge-network containers
// need neither a TCP port exposed on the host nor a second Redis instance.
func (c Client) RedisConn(ctx context.Context) (net.Conn, error) {
	conn, reader, _, err := c.connect(ctx, Request{Operation: "redis"})
	if err != nil {
		return nil, err
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
