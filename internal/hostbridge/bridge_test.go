package hostbridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/require"
)

func TestContainerID(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, path := range []string{"/docker/" + id, "/system.slice/docker-" + id + ".scope", "/user.slice/user-1000.slice/user@1000.service/app.slice/docker-" + id + ".scope", "/docker/" + id + "/child"} {
		require.Equal(t, id, ContainerID("0::"+path))
		require.Equal(t, id, ContainerID("5:cpu,memory:"+path+"\n2:pids:/"))
	}
	for _, value := range []string{"0::/", "0::/docker/abc", "0::/docker/" + id + "suffix"} {
		require.Empty(t, ContainerID(value))
	}
}

func TestOwnerMappingAndDockerLabels(t *testing.T) {
	account, err := user.Current()
	require.NoError(t, err)
	if account.Uid == "0" {
		t.Skip("requires a non-root account for owner mapping")
	}
	id := strings.Repeat("b", 64)
	r := &Resolver{Owners: map[string]string{id: account.Username}}
	owner, err := r.containerOwner(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, account.Username, owner)
	_, err = validOwner("root")
	require.Error(t, err)
	_, err = validOwner("")
	require.Error(t, err)
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '{\"canhazgpu.owner\":\"%s\"}'\n", account.Username)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755))
	t.Setenv("PATH", dir)
	r = &Resolver{}
	owner, err = r.containerOwner(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, account.Username, owner)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\necho '{}'\n"), 0755))
	_, err = r.containerOwner(context.Background(), strings.Repeat("c", 64))
	require.ErrorContains(t, err, "needs canhazgpu.owner")
}

func TestBridgeCredentialsUsageAndRedis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer backend.Close()
	go func() {
		conn, err := backend.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}
	}()
	addr := backend.Addr().(*net.TCPAddr)
	server := &Server{
		Resolver: &Resolver{}, Config: &types.Config{RedisHost: "127.0.0.1", RedisPort: addr.Port, RedisDB: 7, MemoryThreshold: 123},
		Usage: func(context.Context) (map[int]*types.GPUUsage, error) {
			return map[int]*types.GPUUsage{2: {GPUID: 2, Processes: []types.GPUProcessInfo{{PID: 98765, User: "alice"}}}}, nil
		},
		Cancel: func(ctx context.Context, ref, owner string, force bool) (any, error) {
			return map[string]string{"owner": owner, "ref": ref}, nil
		},
	}
	socket := filepath.Join(t.TempDir(), "host.sock")
	require.NoError(t, server.Start(ctx, socket))
	defer server.Close()
	client := Client{Socket: socket}
	identity, err := client.Identity(ctx)
	require.NoError(t, err)
	account, err := user.Current()
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), identity.PID)
	require.Equal(t, account.Username, identity.User)
	require.Equal(t, 7, identity.RedisDB)
	usage, err := client.Usage(ctx)
	require.NoError(t, err)
	require.Equal(t, "alice", usage[2].Processes[0].User)
	var result map[string]string
	require.NoError(t, client.Call(ctx, Request{Operation: "cancel", TaskID: "abc"}, &result))
	require.Equal(t, identity.User, result["owner"])
	if !identity.Admin {
		require.ErrorContains(t, client.Call(ctx, Request{Operation: "cancel", Force: true}, &result), "requires host root")
	}
	conn, err := client.RedisConn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
	_, err = conn.Write([]byte("*1\r\n$4\r\nPING\r\n"))
	require.NoError(t, err)
	data := make([]byte, 14)
	_, err = io.ReadFull(conn, data)
	require.NoError(t, err)
	require.Equal(t, "*1\r\n$4\r\nPING\r\n", string(data))
	require.ErrorContains(t, client.Call(ctx, Request{Operation: "execute"}, &result), "unknown host operation")
}

func TestSocketFailureDoesNotFallBack(t *testing.T) {
	_, err := (Client{Socket: filepath.Join(t.TempDir(), "missing.sock")}).Identity(context.Background())
	require.ErrorContains(t, err, "connect to host guard")
}
