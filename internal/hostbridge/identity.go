package hostbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const OwnerLabel = "canhazgpu.owner"

var containerPattern = regexp.MustCompile(`(?:^|/)(?:docker[-/])?([a-f0-9]{64})(?:\.scope)?(?:/|$)`)

// Resolver runs only on the host. Ownership comes from host configuration or
// Docker metadata, never from USER or a caller-supplied container ID.
type Resolver struct {
	Owners map[string]string
	mu     sync.Mutex
	cache  map[string]ownerCache
}
type ownerCache struct {
	user    string
	expires time.Time
}

func ContainerID(cgroups string) string {
	for _, line := range strings.Split(cgroups, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if m := containerPattern.FindStringSubmatch(parts[2]); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

func (r *Resolver) Resolve(ctx context.Context, pid int) (Identity, error) {
	identity := Identity{PID: pid}
	if pid <= 0 {
		return identity, fmt.Errorf("invalid PID")
	}
	cgroups, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return identity, err
	}
	identity.ContainerID = ContainerID(string(cgroups))
	if identity.ContainerID != "" {
		identity.User, err = r.containerOwner(ctx, identity.ContainerID)
		return identity, err
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return identity, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "Uid:" {
			account, err := user.LookupId(fields[1])
			if err != nil {
				return identity, err
			}
			identity.User = account.Username
			identity.Admin = fields[1] == "0"
			return identity, nil
		}
	}
	return identity, fmt.Errorf("UID missing for PID %d", pid)
}

func (r *Resolver) containerOwner(ctx context.Context, id string) (string, error) {
	if owner := r.Owners[id]; owner != "" {
		return validOwner(owner)
	}
	r.mu.Lock()
	cached, ok := r.cache[id]
	r.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.user, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", "--format", `{{json .Config.Labels}}`, id).Output()
	if err != nil {
		return "", fmt.Errorf("cannot inspect Docker container %s: %w", id[:12], err)
	}
	var labels map[string]string
	if err := json.Unmarshal(out, &labels); err != nil {
		return "", err
	}
	owner, err := validOwner(labels[OwnerLabel])
	if err != nil {
		return "", fmt.Errorf("container %s needs %s=<host-user> or a host owner mapping: %w", id[:12], OwnerLabel, err)
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = make(map[string]ownerCache)
	}
	// Bound memory even on hosts that create many short-lived containers.
	if len(r.cache) >= 1024 {
		clear(r.cache)
	}
	r.cache[id] = ownerCache{owner, time.Now().Add(30 * time.Second)}
	r.mu.Unlock()
	return owner, nil
}

func validOwner(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty owner")
	}
	account, err := user.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("unknown host account %q", name)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid == 0 {
		return "", fmt.Errorf("container owner must be a non-root host account")
	}
	return account.Username, nil
}

func LoadOwners(path string) (map[string]string, error) {
	owners := make(map[string]string)
	if path == "" {
		return owners, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &owners); err != nil {
		return nil, err
	}
	for id, name := range owners {
		if len(id) != 64 || ContainerID("0::/docker/"+id) != id {
			return nil, fmt.Errorf("owner mapping requires full Docker container IDs: %q", id)
		}
		if _, err := validOwner(name); err != nil {
			return nil, err
		}
	}
	return owners, nil
}
