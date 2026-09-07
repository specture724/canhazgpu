package hostbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
)

type Server struct {
	Resolver *Resolver
	Config   *types.Config
	Usage    func(context.Context) (map[int]*types.GPUUsage, error)
	Cancel   func(context.Context, string, string, bool) (any, error)
	listener *net.UnixListener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// Start refuses to replace an existing socket: another guard may own it.
// The directory is mounted, rather than the inode, so clients reconnect after
// a normal guard restart. Access to this socket implies access to shared Redis.
func (s *Server) Start(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0666); err != nil {
		listener.Close()
		return err
	}
	s.listener = listener
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(conn) }()
		}
	}()
	return nil
}

func (s *Server) Close() {
	s.cancel()
	_ = s.listener.Close()
	s.wg.Wait()
}

func (s *Server) serve(conn *net.UnixConn) {
	defer conn.Close()
	stop := context.AfterFunc(s.ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	reader := bufio.NewReader(io.LimitReader(conn, 4096))
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.reply(conn, nil, err)
		return
	}
	pid, err := peerPID(conn)
	if err != nil {
		s.reply(conn, nil, err)
		return
	}
	identity, err := s.Resolver.Resolve(s.ctx, pid)
	if err != nil {
		s.reply(conn, nil, err)
		return
	}
	identity.RedisDB = s.Config.RedisDB
	identity.MemoryThreshold = s.Config.MemoryThreshold
	requestCtx, cancel := context.WithTimeout(s.ctx, 40*time.Second)
	defer cancel()
	switch req.Operation {
	case "identity":
		s.reply(conn, identity, nil)
	case "usage":
		usage, err := s.Usage(requestCtx)
		s.reply(conn, usage, err)
	case "cancel":
		if req.Force && !identity.Admin {
			s.reply(conn, nil, fmt.Errorf("--force requires host root"))
			return
		}
		result, err := s.Cancel(requestCtx, req.TaskID, identity.User, req.Force)
		s.reply(conn, result, err)
	case "redis":
		backend, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(s.ctx, "tcp", net.JoinHostPort(s.Config.RedisHost, fmt.Sprint(s.Config.RedisPort)))
		if err != nil {
			s.reply(conn, nil, err)
			return
		}
		defer backend.Close()
		stopBackend := context.AfterFunc(s.ctx, func() { _ = backend.Close() })
		defer stopBackend()
		if !s.reply(conn, true, nil) {
			return
		}
		_ = conn.SetDeadline(time.Time{})
		// The client waits for the acknowledgement before sending Redis bytes,
		// so the request reader cannot have buffered protocol data.
		done := make(chan struct{})
		go func() { _, _ = io.Copy(backend, conn); _ = backend.Close(); close(done) }()
		_, _ = io.Copy(conn, backend)
		_ = conn.Close()
		<-done
	default:
		s.reply(conn, nil, fmt.Errorf("unknown host operation %q", req.Operation))
	}
}

func (s *Server) reply(conn net.Conn, result any, err error) bool {
	r := response{}
	if err == nil {
		r.Result, err = json.Marshal(result)
	}
	if err != nil {
		r.Error = err.Error()
	}
	return json.NewEncoder(conn).Encode(r) == nil
}
