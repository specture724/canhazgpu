package redis_client

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/russellb/canhazgpu/internal/types"
)

type Client struct {
	rdb    *redis.Client
	config *types.Config
}

func NewClient(config *types.Config) *Client {
	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", config.RedisHost, config.RedisPort),
		DB:   config.RedisDB,

		// Connection health settings to detect and recover from stale connections.
		// This is critical for long-lived processes like the supervisor, where a
		// silently dead TCP connection would cause heartbeat failures and eventual
		// reservation loss.
		DialTimeout:  5 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,

		// Limit pool size and set idle timeout so stale connections are cycled out.
		PoolSize:     3,
		MinIdleConns: 1,
		IdleTimeout:  90 * time.Second,

		// MaxRetries causes go-redis to automatically retry failed commands on
		// transient connection errors (connection reset, timeout, etc.) before
		// returning an error to the caller.
		MaxRetries:      3,
		MinRetryBackoff: 100 * time.Millisecond,
		MaxRetryBackoff: 2 * time.Second,
	})

	return &Client{rdb: rdb, config: config}
}

func (c *Client) Close() error {
	return c.rdb.Close()
}

func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// HealthCheck verifies the Redis connection is alive with a short timeout.
// Returns nil if healthy, an error otherwise.
func (c *Client) HealthCheck(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.rdb.Ping(checkCtx).Err()
}

// Reconnect closes the current connection and creates a new one.
// This is used to recover from stale/dead TCP connections in long-lived
// processes like the supervisor.
func (c *Client) Reconnect() error {
	// Close existing connection (ignore errors from already-broken connections)
	_ = c.rdb.Close()

	c.rdb = redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", c.config.RedisHost, c.config.RedisPort),
		DB:   c.config.RedisDB,

		DialTimeout:  5 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,

		PoolSize:     3,
		MinIdleConns: 1,
		IdleTimeout:  90 * time.Second,

		MaxRetries:      3,
		MinRetryBackoff: 100 * time.Millisecond,
		MaxRetryBackoff: 2 * time.Second,
	})

	// Verify the new connection works
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.rdb.Ping(ctx).Err()
}

// GPU State Management

func (c *Client) SetGPUCount(ctx context.Context, count int) error {
	return c.rdb.Set(ctx, types.RedisKeyGPUCount, count, 0).Err()
}

func (c *Client) GetGPUCount(ctx context.Context) (int, error) {
	val, err := c.rdb.Get(ctx, types.RedisKeyGPUCount).Int()
	if err == redis.Nil {
		return 0, fmt.Errorf("GPU pool not initialized - run 'canhazgpu admin --gpus <count>' first")
	}
	return val, err
}

func (c *Client) SetAvailableProvider(ctx context.Context, provider string) error {
	return c.rdb.Set(ctx, types.RedisKeyProvider, provider, 0).Err()
}

func (c *Client) GetAvailableProvider(ctx context.Context) (string, error) {
	val, err := c.rdb.Get(ctx, types.RedisKeyProvider).Result()
	if err == redis.Nil {
		// Check if this is a pre-provider deployment by looking for existing GPU count
		gpuCount, countErr := c.GetGPUCount(ctx)
		if countErr == nil && gpuCount > 0 {
			// This is a pre-provider deployment - auto-migrate to NVIDIA for backward compatibility
			provider := "nvidia"
			if setErr := c.SetAvailableProvider(ctx, provider); setErr != nil {
				return "", fmt.Errorf("failed to auto-migrate pre-provider deployment to NVIDIA: %v", setErr)
			}
			return provider, nil
		}
		return "", fmt.Errorf("GPU provider not initialized - run 'canhazgpu admin --gpus <count>' first")
	}
	if err != nil {
		return "", err
	}

	return val, nil
}

func (c *Client) GetGPUState(ctx context.Context, gpuID int) (*types.GPUState, error) {
	key := fmt.Sprintf("%sgpu:%d", types.RedisKeyPrefix, gpuID)
	val, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		// GPU is available
		return &types.GPUState{}, nil
	}
	if err != nil {
		return nil, err
	}

	var state types.GPUState
	if err := json.Unmarshal([]byte(val), &state); err != nil {
		return nil, fmt.Errorf("corrupted GPU state for GPU %d: %v", gpuID, err)
	}

	return &state, nil
}

func (c *Client) SetGPUState(ctx context.Context, gpuID int, state *types.GPUState) error {
	key := fmt.Sprintf("%sgpu:%d", types.RedisKeyPrefix, gpuID)

	if state.User == "" {
		// GPU is available, keep the timestamps that describe when it was last
		// occupied: last_released (from a reservation) and last_activity (from
		// unreserved usage). The latter drives the "free for" clock after
		// unreserved processes stop.
		availableState := types.GPUState{
			LastReleased: state.LastReleased,
			LastActivity: state.LastActivity,
		}
		if !availableState.LastReleased.ToTime().IsZero() || !availableState.LastActivity.ToTime().IsZero() {
			data, err := json.Marshal(availableState)
			if err != nil {
				return err
			}
			return c.rdb.Set(ctx, key, data, 0).Err()
		}
		// Delete the key if no useful state
		return c.rdb.Del(ctx, key).Err()
	}

	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	return c.rdb.Set(ctx, key, data, 0).Err()
}

func (c *Client) DeleteGPUState(ctx context.Context, gpuID int) error {
	key := fmt.Sprintf("%sgpu:%d", types.RedisKeyPrefix, gpuID)
	return c.rdb.Del(ctx, key).Err()
}

// Allocation Lock Management

func (c *Client) AcquireAllocationLock(ctx context.Context) error {
	for attempt := 0; attempt < types.MaxLockRetries; attempt++ {
		acquired, err := c.rdb.SetNX(ctx, types.RedisKeyAllocationLock, "locked", types.LockTimeout).Result()
		if err != nil {
			return err
		}
		if acquired {
			return nil
		}

		// Exponential backoff with jitter
		sleepTime := time.Duration(1<<attempt)*time.Second + time.Duration(rand.Intn(1000))*time.Millisecond
		time.Sleep(sleepTime)
	}

	return fmt.Errorf("failed to acquire allocation lock after %d attempts", types.MaxLockRetries)
}

func (c *Client) ReleaseAllocationLock(ctx context.Context) error {
	return c.rdb.Del(ctx, types.RedisKeyAllocationLock).Err()
}

// Atomic GPU Allocation using Lua script
func (c *Client) AtomicReserveGPUs(ctx context.Context, request *types.AllocationRequest, unreservedGPUs []int) ([]int, error) {
	// Check if specific GPU IDs are requested
	if len(request.GPUIDs) > 0 {
		return c.atomicReserveSpecificGPUs(ctx, request, unreservedGPUs)
	}

	// MRU-per-user logic for allocating by count
	luaScript := `
		local gpu_count = tonumber(ARGV[1])
		local requested = tonumber(ARGV[2])
		local user = ARGV[3]
		local actual_user = ARGV[4]
		local reservation_type = ARGV[5]
		local current_time = tonumber(ARGV[6])
		local expiry_time = ARGV[7]
		local unreserved_gpus_json = ARGV[8]
		local note = ARGV[9]
		local blocked_gpus_json = ARGV[10]
		local idle_timeout = tonumber(ARGV[11])
		local task_id = ARGV[12]
		local pid = tonumber(ARGV[13])

		-- Parse unreserved GPUs
		local unreserved_gpus = {}
		if unreserved_gpus_json and unreserved_gpus_json ~= "" and unreserved_gpus_json ~= "[]" and unreserved_gpus_json ~= "null" then
			local success, unreserved_list = pcall(cjson.decode, unreserved_gpus_json)
			if success and unreserved_list and type(unreserved_list) == "table" then
				for _, gpu_id in ipairs(unreserved_list) do
					unreserved_gpus[tonumber(gpu_id)] = true
				end
			end
		end

		-- Parse GPUs held back for upcoming scheduled bookings
		local blocked_gpus = {}
		if blocked_gpus_json and blocked_gpus_json ~= "" and blocked_gpus_json ~= "[]" and blocked_gpus_json ~= "null" then
			local success, blocked_list = pcall(cjson.decode, blocked_gpus_json)
			if success and blocked_list and type(blocked_list) == "table" then
				for _, gpu_id in ipairs(blocked_list) do
					blocked_gpus[tonumber(gpu_id)] = true
				end
			end
		end

		-- Query usage history for this user to get their most recently used GPUs
		local user_gpu_history = {}
		local history_key = "canhazgpu:usage_history_sorted"

		-- Get recent usage records for this user (last 100 records should be plenty)
		local recent_records = redis.call('ZREVRANGE', history_key, 0, 99, 'WITHSCORES')
		for i = 1, #recent_records, 2 do
			local record_json = recent_records[i]
			local timestamp = tonumber(recent_records[i + 1])

			local success, record = pcall(cjson.decode, record_json)
			if success and record and record.user == user and record.gpu_id ~= nil then
				local gpu_id = tonumber(record.gpu_id)
				-- Only keep the most recent timestamp for each GPU
				if not user_gpu_history[gpu_id] or user_gpu_history[gpu_id] < timestamp then
					user_gpu_history[gpu_id] = timestamp
				end
			end
		end

		-- Get available GPUs with MRU-per-user ranking
		local available_gpus = {}
		for i = 0, gpu_count - 1 do
			local key = "canhazgpu:gpu:" .. i
			local gpu_data = redis.call('GET', key)

			-- Skip GPUs in unreserved use or held for a scheduled booking
			if not unreserved_gpus[i] and not blocked_gpus[i] then
				if not gpu_data then
					-- GPU is available (never used)
					table.insert(available_gpus, {
						id = i,
						last_released = 0,
						user_last_used = user_gpu_history[i] or 0
					})
				else
					local state = cjson.decode(gpu_data)
					if not state.user then
						-- GPU is available
						local last_released = 0

						-- Parse last_released timestamp
						if state.last_released and state.last_released ~= "" then
							-- RFC3339 format: extract Unix timestamp
							-- Try to convert RFC3339 to seconds since epoch
							-- For simplicity, we'll use the current_time as a reference
							-- and assign a large value to indicate it was previously used
							last_released = current_time - 86400 -- Default to 24 hours ago

							-- Better approach: extract year, month, day, hour, minute, second from RFC3339
							-- Format: 2025-06-30T16:34:38.372177993Z
							local year, month, day, hour, min, sec = string.match(state.last_released,
								"(%d+)-(%d+)-(%d+)T(%d+):(%d+):(%d+)")

							if year then
								-- Convert to Unix timestamp (approximate)
								-- This is a simplified conversion that works for recent dates
								local days_since_epoch = (tonumber(year) - 1970) * 365 +
									(tonumber(month) - 1) * 30 +
									tonumber(day)
								last_released = days_since_epoch * 86400 +
									tonumber(hour) * 3600 +
									tonumber(min) * 60 +
									tonumber(sec)
							end
						end

						table.insert(available_gpus, {
							id = i,
							last_released = last_released,
							user_last_used = user_gpu_history[i] or 0
						})
					end
				end
			end
		end

		-- Sort by MRU-per-user: prefer GPUs this user used most recently
		-- If user never used a GPU, fall back to global LRU
		table.sort(available_gpus, function(a, b)
			-- If both have user history, prefer more recent
			if a.user_last_used > 0 and b.user_last_used > 0 then
				return a.user_last_used > b.user_last_used
			end
			-- If only one has user history, prefer it
			if a.user_last_used > 0 then
				return true
			end
			if b.user_last_used > 0 then
				return false
			end
			-- Neither has user history, use global LRU (oldest first)
			return a.last_released < b.last_released
		end)
		
		-- Check if we have enough GPUs
		if #available_gpus < requested then
			return redis.error_reply("Not enough GPUs available")
		end
		
		-- Allocate requested GPUs
		local allocated = {}
		for i = 1, requested do
			local gpu_id = available_gpus[i].id
			table.insert(allocated, gpu_id)

			-- Create reservation state
			local state = {
				user = user,
				actual_user = actual_user,
				start_time = current_time,
				type = reservation_type
			}

			if reservation_type == "run" then
				state.last_heartbeat = current_time
			elseif reservation_type == "manual" then
				if expiry_time ~= "nil" then
					state.expiry_time = tonumber(expiry_time)
				end
				-- Idle timeout only applies to manual reservations: run-type
				-- reservations are already tied to the lifetime of a process
				if idle_timeout and idle_timeout > 0 then
					state.idle_timeout = idle_timeout
					state.last_activity = current_time
				end
			end

			-- Add note if provided
			if note and note ~= "" then
				state.note = note
			end

			-- Handle used by 'queue' and 'cancel'
			if task_id and task_id ~= "" then
				state.task_id = task_id
			end
			if pid and pid > 0 then
				state.pid = pid
			end

			-- Set GPU state
			local key = "canhazgpu:gpu:" .. gpu_id
			redis.call('SET', key, cjson.encode(state))
		end
		
		return allocated
	`

	// Convert unreserved GPUs to JSON
	unreservedJSON, err := json.Marshal(unreservedGPUs)
	if err != nil {
		return nil, err
	}

	// Convert GPUs held for scheduled bookings to JSON
	blockedJSON, err := json.Marshal(request.BlockedGPUs)
	if err != nil {
		return nil, err
	}

	// Get GPU count
	gpuCount, err := c.GetGPUCount(ctx)
	if err != nil {
		return nil, err
	}

	// Prepare arguments
	currentTime := time.Now().Unix()
	expiryTime := "nil"
	if request.ExpiryTime != nil {
		expiryTime = fmt.Sprintf("%d", request.ExpiryTime.Unix())
	}

	// Execute Lua script
	result, err := c.rdb.Eval(ctx, luaScript, []string{},
		gpuCount,
		request.GPUCount,
		request.User,
		request.ActualUser,
		request.ReservationType,
		currentTime,
		expiryTime,
		string(unreservedJSON),
		request.Note,
		string(blockedJSON),
		int64(request.IdleTimeout.Seconds()),
		request.TaskID,
		request.PID,
	).Result()

	if err != nil {
		return nil, err
	}

	// Parse result
	switch v := result.(type) {
	case []interface{}:
		// Check if first element is an error map
		if len(v) > 0 {
			if errMap, ok := v[0].(map[string]interface{}); ok {
				if errorMsg, hasError := errMap["error"]; hasError {
					return nil, fmt.Errorf("%v", errorMsg)
				}
			}
		}

		// Parse allocated GPU IDs
		var allocated []int
		for _, item := range v {
			if gpuID, ok := item.(int64); ok {
				allocated = append(allocated, int(gpuID))
			}
		}
		return allocated, nil
	case map[string]interface{}:
		// Handle error result directly as a map
		if errorMsg, hasError := v["error"]; hasError {
			return nil, fmt.Errorf("%v", errorMsg)
		}
		return nil, fmt.Errorf("unexpected map result from Lua script: %v", v)
	default:
		return nil, fmt.Errorf("unexpected result type from Lua script: %T", result)
	}
}

// atomicReserveSpecificGPUs reserves specific GPU IDs if they are available
func (c *Client) atomicReserveSpecificGPUs(ctx context.Context, request *types.AllocationRequest, unreservedGPUs []int) ([]int, error) {
	luaScript := `
		local requested_gpus_json = ARGV[1]
		local user = ARGV[2]
		local actual_user = ARGV[3]
		local reservation_type = ARGV[4]
		local current_time = tonumber(ARGV[5])
		local expiry_time = ARGV[6]
		local unreserved_gpus_json = ARGV[7]
		local gpu_count = tonumber(ARGV[8])
		local note = ARGV[9]
		local blocked_gpus_json = ARGV[10]
		local idle_timeout = tonumber(ARGV[11])
		local task_id = ARGV[12]
		local pid = tonumber(ARGV[13])

		-- Parse requested GPU IDs
		local requested_gpus = {}
		if requested_gpus_json and requested_gpus_json ~= "" and requested_gpus_json ~= "[]" and requested_gpus_json ~= "null" then
			local success, gpu_list = pcall(cjson.decode, requested_gpus_json)
			if not success or not gpu_list or type(gpu_list) ~= "table" then
				return redis.error_reply("Invalid GPU IDs format")
			end
			requested_gpus = gpu_list
		else
			return redis.error_reply("No GPU IDs specified")
		end
		
		-- Parse unreserved GPUs (GPUs in use without reservation)
		local unreserved_gpus = {}
		if unreserved_gpus_json and unreserved_gpus_json ~= "" and unreserved_gpus_json ~= "[]" and unreserved_gpus_json ~= "null" then
			local success, unreserved_list = pcall(cjson.decode, unreserved_gpus_json)
			if success and unreserved_list and type(unreserved_list) == "table" then
				for _, gpu_id in ipairs(unreserved_list) do
					unreserved_gpus[tonumber(gpu_id)] = true
				end
			end
		end
		
		-- Parse GPUs held back for upcoming scheduled bookings
		local blocked_gpus = {}
		if blocked_gpus_json and blocked_gpus_json ~= "" and blocked_gpus_json ~= "[]" and blocked_gpus_json ~= "null" then
			local success, blocked_list = pcall(cjson.decode, blocked_gpus_json)
			if success and blocked_list and type(blocked_list) == "table" then
				for _, gpu_id in ipairs(blocked_list) do
					blocked_gpus[tonumber(gpu_id)] = true
				end
			end
		end

		-- Validate all requested GPUs
		for _, gpu_id in ipairs(requested_gpus) do
			local gpu_id_num = tonumber(gpu_id)

			-- Check if GPU ID is valid (within range)
			if gpu_id_num < 0 or gpu_id_num >= gpu_count then
				return redis.error_reply("GPU ID " .. gpu_id .. " is out of range (0-" .. (gpu_count-1) .. ")")
			end

			-- Check if GPU is unreserved (in use without reservation)
			if unreserved_gpus[gpu_id_num] then
				return redis.error_reply("GPU " .. gpu_id .. " is in use without reservation")
			end

			-- Check if GPU is held for an upcoming scheduled booking
			if blocked_gpus[gpu_id_num] then
				return redis.error_reply("GPU " .. gpu_id .. " is held for a scheduled booking")
			end

			-- Check if GPU is already reserved
			local key = "canhazgpu:gpu:" .. gpu_id
			local gpu_data = redis.call('GET', key)
			
			if gpu_data then
				local state = cjson.decode(gpu_data)
				if state.user then
					-- GPU is already reserved
					if state.type == "manual" and state.expiry_time and tonumber(state.expiry_time) < current_time then
						-- Manual reservation has expired, continue
					elseif state.type == "run" and state.last_heartbeat and (current_time - tonumber(state.last_heartbeat)) > 300 then
						-- Run reservation heartbeat timeout (5 minutes), continue
					else
						-- GPU is actively reserved
						return redis.error_reply("GPU " .. gpu_id .. " is already reserved by user '" .. state.user .. "'")
					end
				end
			end
		end
		
		-- All GPUs are available, reserve them
		local allocated = {}
		for _, gpu_id in ipairs(requested_gpus) do
			local gpu_id_num = tonumber(gpu_id)
			table.insert(allocated, gpu_id_num)

			-- Create reservation state
			local state = {
				user = user,
				actual_user = actual_user,
				start_time = current_time,
				type = reservation_type
			}

			if reservation_type == "run" then
				state.last_heartbeat = current_time
			elseif reservation_type == "manual" then
				if expiry_time ~= "nil" then
					state.expiry_time = tonumber(expiry_time)
				end
				-- Idle timeout only applies to manual reservations: run-type
				-- reservations are already tied to the lifetime of a process
				if idle_timeout and idle_timeout > 0 then
					state.idle_timeout = idle_timeout
					state.last_activity = current_time
				end
			end

			-- Add note if provided
			if note and note ~= "" then
				state.note = note
			end

			-- Handle used by 'queue' and 'cancel'
			if task_id and task_id ~= "" then
				state.task_id = task_id
			end
			if pid and pid > 0 then
				state.pid = pid
			end

			-- Set GPU state
			local key = "canhazgpu:gpu:" .. gpu_id
			redis.call('SET', key, cjson.encode(state))
		end
		
		return allocated
	`

	// Convert requested GPU IDs to JSON
	requestedGPUsJSON, err := json.Marshal(request.GPUIDs)
	if err != nil {
		return nil, err
	}

	// Convert unreserved GPUs to JSON
	unreservedJSON, err := json.Marshal(unreservedGPUs)
	if err != nil {
		return nil, err
	}

	// Convert GPUs held for scheduled bookings to JSON
	blockedJSON, err := json.Marshal(request.BlockedGPUs)
	if err != nil {
		return nil, err
	}

	// Get GPU count for validation
	gpuCount, err := c.GetGPUCount(ctx)
	if err != nil {
		return nil, err
	}

	// Prepare arguments
	currentTime := time.Now().Unix()
	expiryTime := "nil"
	if request.ExpiryTime != nil {
		expiryTime = fmt.Sprintf("%d", request.ExpiryTime.Unix())
	}

	// Execute Lua script
	result, err := c.rdb.Eval(ctx, luaScript, []string{},
		string(requestedGPUsJSON),
		request.User,
		request.ActualUser,
		request.ReservationType,
		currentTime,
		expiryTime,
		string(unreservedJSON),
		gpuCount,
		request.Note,
		string(blockedJSON),
		int64(request.IdleTimeout.Seconds()),
		request.TaskID,
		request.PID,
	).Result()

	if err != nil {
		return nil, err
	}

	// Parse result
	switch v := result.(type) {
	case []interface{}:
		// Check if first element is an error map
		if len(v) > 0 {
			if errorMap, ok := v[0].(map[string]interface{}); ok {
				if errorMsg, hasError := errorMap["error"]; hasError {
					return nil, fmt.Errorf("%v", errorMsg)
				}
			}
		}

		// Convert to int slice
		var allocatedGPUs []int
		for _, id := range v {
			if gpuID, ok := id.(int64); ok {
				allocatedGPUs = append(allocatedGPUs, int(gpuID))
			}
		}
		return allocatedGPUs, nil
	case map[string]interface{}:
		// Handle error result directly as a map
		if errorMsg, hasError := v["error"]; hasError {
			return nil, fmt.Errorf("%v", errorMsg)
		}
		return nil, fmt.Errorf("unexpected map result from Lua script: %v", v)
	default:
		return nil, fmt.Errorf("unexpected result type from Lua script: %T", result)
	}
}

// Clear all GPU states (for admin --force)
func (c *Client) ClearAllGPUStates(ctx context.Context) error {
	// Get all GPU keys
	keys, err := c.rdb.Keys(ctx, types.RedisKeyPrefix+"gpu:*").Result()
	if err != nil {
		return err
	}

	if len(keys) > 0 {
		return c.rdb.Del(ctx, keys...).Err()
	}

	return nil
}

// FlushTestDB flushes the entire test database (DB 15).
// This should only be used in tests to ensure a clean state.
func (c *Client) FlushTestDB(ctx context.Context) error {
	return c.rdb.FlushDB(ctx).Err()
}

// DeleteAllKeys removes every key this tool owns in the current database,
// leaving keys belonging to anything else that shares the database untouched.
// It returns how many keys were removed.
func (c *Client) DeleteAllKeys(ctx context.Context) (int, error) {
	var deleted int
	var cursor uint64

	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, types.RedisKeyPrefix+"*", 500).Result()
		if err != nil {
			return deleted, err
		}

		if len(keys) > 0 {
			removed, err := c.rdb.Del(ctx, keys...).Result()
			if err != nil {
				return deleted, err
			}
			deleted += int(removed)
		}

		cursor = next
		if cursor == 0 {
			return deleted, nil
		}
	}
}

// RecordUsageHistory records a GPU usage entry when a reservation is released
func (c *Client) RecordUsageHistory(ctx context.Context, record *types.UsageRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	// Write to new sorted set format for efficient range queries
	sortedSetKey := types.RedisKeyPrefix + "usage_history_sorted"
	score := float64(record.EndTime.ToTime().Unix())

	// Add to sorted set with timestamp as score
	if err := c.rdb.ZAdd(ctx, sortedSetKey, &redis.Z{
		Score:  score,
		Member: string(data),
	}).Err(); err != nil {
		return fmt.Errorf("failed to add to sorted set: %v", err)
	}

	// Set expiration on sorted set (90 days) if not already set
	if err := c.rdb.Expire(ctx, sortedSetKey, 90*24*time.Hour).Err(); err != nil {
		// Log warning but don't fail - expiration might already be set
		fmt.Printf("Warning: failed to set expiration on usage history: %v\n", err)
	}

	return nil
}

// GetUsageHistory retrieves usage history for the specified time range
func (c *Client) GetUsageHistory(ctx context.Context, startTime, endTime time.Time) ([]*types.UsageRecord, error) {
	sortedSetKey := types.RedisKeyPrefix + "usage_history_sorted"

	// Check if new sorted set format exists
	exists, err := c.rdb.Exists(ctx, sortedSetKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to check sorted set existence: %v", err)
	}

	if exists > 0 {
		// Use efficient sorted set range query
		results, err := c.rdb.ZRangeByScore(ctx, sortedSetKey, &redis.ZRangeBy{
			Min: fmt.Sprintf("%d", startTime.Unix()),
			Max: fmt.Sprintf("%d", endTime.Unix()),
		}).Result()
		if err != nil {
			return nil, fmt.Errorf("failed to query sorted set: %v", err)
		}

		var records []*types.UsageRecord
		for _, result := range results {
			var record types.UsageRecord
			if err := json.Unmarshal([]byte(result), &record); err != nil {
				continue
			}
			records = append(records, &record)
		}

		return records, nil
	}

	// TODO: Remove backwards compatibility fallback after migration is complete
	// New format doesn't exist - check old format and migrate
	oldRecords, err := c.getUsageHistoryOldFormat(ctx, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve usage history from old format: %v", err)
	}

	// If we found old records, migrate them to new format but leave old data in place
	// TODO: Add cleanup of old data after confident migration is successful
	if len(oldRecords) > 0 {
		if err := c.migrateOldUsageRecords(ctx, oldRecords); err != nil {
			// Log warning but still return the old records
			fmt.Printf("Warning: failed to migrate old usage records: %v\n", err)
		}
	}

	return oldRecords, nil
}

// getUsageHistoryOldFormat retrieves usage history using the old KEYS-based approach
// This function is used for backwards compatibility during migration
func (c *Client) getUsageHistoryOldFormat(ctx context.Context, startTime, endTime time.Time) ([]*types.UsageRecord, error) {
	// Get all usage history keys using the old pattern
	pattern := types.RedisKeyUsageHistory + "*"
	keys, err := c.rdb.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, err
	}

	var records []*types.UsageRecord
	for _, key := range keys {
		data, err := c.rdb.Get(ctx, key).Result()
		if err != nil {
			continue
		}

		var record types.UsageRecord
		if err := json.Unmarshal([]byte(data), &record); err != nil {
			continue
		}

		// Filter by time range - use <= and >= to be inclusive
		if record.EndTime.ToTime().After(startTime) && record.EndTime.ToTime().Before(endTime.Add(time.Second)) {
			records = append(records, &record)
		}
	}

	return records, nil
}

// migrateOldUsageRecords migrates old format usage records to the new sorted set format
func (c *Client) migrateOldUsageRecords(ctx context.Context, records []*types.UsageRecord) error {
	sortedSetKey := types.RedisKeyPrefix + "usage_history_sorted"

	// Batch add records to sorted set
	var members []*redis.Z
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			continue
		}

		members = append(members, &redis.Z{
			Score:  float64(record.EndTime.ToTime().Unix()),
			Member: string(data),
		})
	}

	if len(members) > 0 {
		if err := c.rdb.ZAdd(ctx, sortedSetKey, members...).Err(); err != nil {
			return fmt.Errorf("failed to migrate records to sorted set: %v", err)
		}

		// Set expiration on sorted set (90 days)
		if err := c.rdb.Expire(ctx, sortedSetKey, 90*24*time.Hour).Err(); err != nil {
			// Log warning but don't fail
			fmt.Printf("Warning: failed to set expiration on usage history: %v\n", err)
		}
	}

	return nil
}

// Booking Management Operations

// SaveBooking stores a booking, indexed by its start time. It is used both for
// creating new bookings and for updating existing ones.
func (c *Client) SaveBooking(ctx context.Context, booking *types.Booking) error {
	data, err := json.Marshal(booking)
	if err != nil {
		return fmt.Errorf("failed to marshal booking: %v", err)
	}

	pipe := c.rdb.TxPipeline()
	pipe.ZAdd(ctx, types.RedisKeyBookings, &redis.Z{
		Score:  float64(booking.StartTime.ToTime().Unix()),
		Member: booking.ID,
	})
	pipe.Set(ctx, types.RedisKeyBooking+booking.ID, data, 0)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to save booking: %v", err)
	}

	return nil
}

// GetBooking retrieves a booking by its full ID. It returns nil (without an
// error) when no such booking exists.
func (c *Client) GetBooking(ctx context.Context, bookingID string) (*types.Booking, error) {
	data, err := c.rdb.Get(ctx, types.RedisKeyBooking+bookingID).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get booking: %v", err)
	}

	var booking types.Booking
	if err := json.Unmarshal([]byte(data), &booking); err != nil {
		return nil, fmt.Errorf("corrupted booking %s: %v", bookingID, err)
	}

	return &booking, nil
}

// DeleteBooking removes a booking entirely
func (c *Client) DeleteBooking(ctx context.Context, bookingID string) error {
	pipe := c.rdb.TxPipeline()
	pipe.ZRem(ctx, types.RedisKeyBookings, bookingID)
	pipe.Del(ctx, types.RedisKeyBooking+bookingID)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to delete booking: %v", err)
	}

	return nil
}

// GetAllBookings returns every stored booking ordered by start time
func (c *Client) GetAllBookings(ctx context.Context) ([]*types.Booking, error) {
	bookingIDs, err := c.rdb.ZRange(ctx, types.RedisKeyBookings, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get booking IDs: %v", err)
	}

	var bookings []*types.Booking
	for _, bookingID := range bookingIDs {
		booking, err := c.GetBooking(ctx, bookingID)
		if err != nil {
			continue
		}
		if booking == nil {
			// Index entry without details - drop it so the index stays clean
			_ = c.rdb.ZRem(ctx, types.RedisKeyBookings, bookingID).Err()
			continue
		}
		bookings = append(bookings, booking)
	}

	return bookings, nil
}

// Violation Management Operations

// SaveViolation stores an open violation, indexed by when it was first seen
func (c *Client) SaveViolation(ctx context.Context, violation *types.Violation) error {
	data, err := json.Marshal(violation)
	if err != nil {
		return fmt.Errorf("failed to marshal violation: %v", err)
	}

	pipe := c.rdb.TxPipeline()
	pipe.ZAdd(ctx, types.RedisKeyViolations, &redis.Z{
		Score:  float64(violation.FirstSeen.ToTime().Unix()),
		Member: violation.ViolationID(),
	})
	pipe.Set(ctx, types.RedisKeyViolation+violation.ViolationID(), data, 24*time.Hour)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to save violation: %v", err)
	}

	return nil
}

// GetViolation retrieves an open violation by its "gpu:pid" identifier. It
// returns nil (without an error) when there is no such violation.
func (c *Client) GetViolation(ctx context.Context, violationID string) (*types.Violation, error) {
	data, err := c.rdb.Get(ctx, types.RedisKeyViolation+violationID).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get violation: %v", err)
	}

	var violation types.Violation
	if err := json.Unmarshal([]byte(data), &violation); err != nil {
		return nil, fmt.Errorf("corrupted violation %s: %v", violationID, err)
	}

	return &violation, nil
}

// DeleteViolation removes an open violation
func (c *Client) DeleteViolation(ctx context.Context, violationID string) error {
	pipe := c.rdb.TxPipeline()
	pipe.ZRem(ctx, types.RedisKeyViolations, violationID)
	pipe.Del(ctx, types.RedisKeyViolation+violationID)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to delete violation: %v", err)
	}

	return nil
}

// GetAllViolations returns every open violation, oldest first
func (c *Client) GetAllViolations(ctx context.Context) ([]*types.Violation, error) {
	violationIDs, err := c.rdb.ZRange(ctx, types.RedisKeyViolations, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get violation IDs: %v", err)
	}

	var violations []*types.Violation
	for _, violationID := range violationIDs {
		violation, err := c.GetViolation(ctx, violationID)
		if err != nil {
			continue
		}
		if violation == nil {
			// Details expired - drop the index entry as well
			_ = c.rdb.ZRem(ctx, types.RedisKeyViolations, violationID).Err()
			continue
		}
		violations = append(violations, violation)
	}

	return violations, nil
}

// RecordViolationHistory appends a resolved violation to the history
func (c *Client) RecordViolationHistory(ctx context.Context, violation *types.Violation) error {
	data, err := json.Marshal(violation)
	if err != nil {
		return err
	}

	if err := c.rdb.ZAdd(ctx, types.RedisKeyViolationHistory, &redis.Z{
		Score:  float64(violation.EndTime.ToTime().Unix()),
		Member: string(data),
	}).Err(); err != nil {
		return fmt.Errorf("failed to record violation history: %v", err)
	}

	if err := c.rdb.Expire(ctx, types.RedisKeyViolationHistory, types.ViolationRetention).Err(); err != nil {
		fmt.Printf("Warning: failed to set expiration on violation history: %v\n", err)
	}

	return nil
}

// GetViolationHistory retrieves resolved violations in the given time range
func (c *Client) GetViolationHistory(ctx context.Context, startTime, endTime time.Time) ([]*types.Violation, error) {
	results, err := c.rdb.ZRangeByScore(ctx, types.RedisKeyViolationHistory, &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", startTime.Unix()),
		Max: fmt.Sprintf("%d", endTime.Unix()),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to query violation history: %v", err)
	}

	var violations []*types.Violation
	for _, result := range results {
		var violation types.Violation
		if err := json.Unmarshal([]byte(result), &violation); err != nil {
			continue
		}
		violations = append(violations, &violation)
	}

	return violations, nil
}

// Guard Coordination

// AcquireGuardLock claims the singleton guard role. It reports whether the lock
// was obtained, and who holds it otherwise.
func (c *Client) AcquireGuardLock(ctx context.Context, owner string) (bool, string, error) {
	acquired, err := c.rdb.SetNX(ctx, types.RedisKeyGuardLock, owner, types.GuardLockTTL).Result()
	if err != nil {
		return false, "", err
	}
	if acquired {
		return true, owner, nil
	}

	holder, err := c.rdb.Get(ctx, types.RedisKeyGuardLock).Result()
	if err == redis.Nil {
		// The lock expired between the two calls - let the caller retry
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}

	// A guard that restarts quickly should be able to reclaim its own lock
	if holder == owner {
		return true, owner, c.rdb.Expire(ctx, types.RedisKeyGuardLock, types.GuardLockTTL).Err()
	}

	return false, holder, nil
}

// RefreshGuardLock extends the guard lock, but only while this owner holds it
func (c *Client) RefreshGuardLock(ctx context.Context, owner string) error {
	luaScript := `
		if redis.call('GET', KEYS[1]) == ARGV[1] then
			return redis.call('PEXPIRE', KEYS[1], ARGV[2])
		end
		return 0
	`

	result, err := c.rdb.Eval(ctx, luaScript, []string{types.RedisKeyGuardLock},
		owner, types.GuardLockTTL.Milliseconds()).Result()
	if err != nil {
		return err
	}

	if refreshed, ok := result.(int64); ok && refreshed == 0 {
		return fmt.Errorf("guard lock is no longer held by %s", owner)
	}

	return nil
}

// ReleaseGuardLock releases the guard lock if this owner still holds it
func (c *Client) ReleaseGuardLock(ctx context.Context, owner string) error {
	luaScript := `
		if redis.call('GET', KEYS[1]) == ARGV[1] then
			return redis.call('DEL', KEYS[1])
		end
		return 0
	`

	return c.rdb.Eval(ctx, luaScript, []string{types.RedisKeyGuardLock}, owner).Err()
}

// RecordGuardKill notes that the guard terminated a process, for rate limiting
func (c *Client) RecordGuardKill(ctx context.Context, when time.Time, description string) error {
	if err := c.rdb.ZAdd(ctx, types.RedisKeyGuardKills, &redis.Z{
		Score:  float64(when.UnixNano()),
		Member: fmt.Sprintf("%d %s", when.UnixNano(), description),
	}).Err(); err != nil {
		return err
	}

	return c.rdb.Expire(ctx, types.RedisKeyGuardKills, 24*time.Hour).Err()
}

// CountGuardKillsSince returns how many processes the guard terminated since
// the given time
func (c *Client) CountGuardKillsSince(ctx context.Context, since time.Time) (int, error) {
	count, err := c.rdb.ZCount(ctx, types.RedisKeyGuardKills,
		fmt.Sprintf("%d", since.UnixNano()), "+inf").Result()
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

// Queue Management Operations

// AddToQueue adds a new entry to the queue
func (c *Client) AddToQueue(ctx context.Context, entry *types.QueueEntry) error {
	// Store entry details
	entryKey := types.RedisKeyQueueEntry + entry.ID
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to marshal queue entry: %v", err)
	}

	// Use a transaction to add both the sorted set entry and the details atomically
	pipe := c.rdb.TxPipeline()

	// Add to sorted set with enqueue timestamp as score
	score := float64(entry.EnqueueTime.ToTime().UnixNano())
	pipe.ZAdd(ctx, types.RedisKeyQueue, &redis.Z{
		Score:  score,
		Member: entry.ID,
	})

	// Store entry details
	pipe.Set(ctx, entryKey, data, 0)

	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to add to queue: %v", err)
	}

	return nil
}

// RemoveFromQueue removes an entry from the queue
func (c *Client) RemoveFromQueue(ctx context.Context, queueID string) error {
	entryKey := types.RedisKeyQueueEntry + queueID

	// Use a transaction to remove both atomically
	pipe := c.rdb.TxPipeline()
	pipe.ZRem(ctx, types.RedisKeyQueue, queueID)
	pipe.Del(ctx, entryKey)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to remove from queue: %v", err)
	}

	return nil
}

// GetQueueEntry retrieves a queue entry by ID
func (c *Client) GetQueueEntry(ctx context.Context, queueID string) (*types.QueueEntry, error) {
	entryKey := types.RedisKeyQueueEntry + queueID
	data, err := c.rdb.Get(ctx, entryKey).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get queue entry: %v", err)
	}

	var entry types.QueueEntry
	if err := json.Unmarshal([]byte(data), &entry); err != nil {
		return nil, fmt.Errorf("failed to unmarshal queue entry: %v", err)
	}

	return &entry, nil
}

// UpdateQueueEntry updates a queue entry
func (c *Client) UpdateQueueEntry(ctx context.Context, entry *types.QueueEntry) error {
	entryKey := types.RedisKeyQueueEntry + entry.ID
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to marshal queue entry: %v", err)
	}

	return c.rdb.Set(ctx, entryKey, data, 0).Err()
}

// UpdateQueueEntryHeartbeat updates the heartbeat timestamp for a queue entry
func (c *Client) UpdateQueueEntryHeartbeat(ctx context.Context, queueID string) error {
	entry, err := c.GetQueueEntry(ctx, queueID)
	if err != nil {
		return err
	}
	if entry == nil {
		return fmt.Errorf("queue entry not found: %s", queueID)
	}

	entry.LastHeartbeat = types.FlexibleTime{Time: time.Now()}
	return c.UpdateQueueEntry(ctx, entry)
}

// GetAllQueueEntries returns all queue entries in order (oldest first)
func (c *Client) GetAllQueueEntries(ctx context.Context) ([]*types.QueueEntry, error) {
	// Get all queue IDs in order
	queueIDs, err := c.rdb.ZRange(ctx, types.RedisKeyQueue, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get queue IDs: %v", err)
	}

	var entries []*types.QueueEntry
	for _, queueID := range queueIDs {
		entry, err := c.GetQueueEntry(ctx, queueID)
		if err != nil {
			continue
		}
		if entry != nil {
			entries = append(entries, entry)
		}
	}

	return entries, nil
}

// GetQueuePosition returns the 0-based position of an entry in the queue
// Returns -1 if not found
func (c *Client) GetQueuePosition(ctx context.Context, queueID string) (int, error) {
	rank, err := c.rdb.ZRank(ctx, types.RedisKeyQueue, queueID).Result()
	if err == redis.Nil {
		return -1, nil
	}
	if err != nil {
		return -1, fmt.Errorf("failed to get queue position: %v", err)
	}

	return int(rank), nil
}

// GetQueueLength returns the number of entries in the queue
func (c *Client) GetQueueLength(ctx context.Context) (int, error) {
	count, err := c.rdb.ZCard(ctx, types.RedisKeyQueue).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to get queue length: %v", err)
	}
	return int(count), nil
}

// IsFirstInQueue checks if the given entry is first in the queue
func (c *Client) IsFirstInQueue(ctx context.Context, queueID string) (bool, error) {
	// Get the first entry in the queue
	firstEntries, err := c.rdb.ZRange(ctx, types.RedisKeyQueue, 0, 0).Result()
	if err != nil {
		return false, fmt.Errorf("failed to get first queue entry: %v", err)
	}

	if len(firstEntries) == 0 {
		return false, nil
	}

	return firstEntries[0] == queueID, nil
}

// CleanupStaleQueueEntries removes queue entries with expired heartbeats
// and releases any partial allocations they held
func (c *Client) CleanupStaleQueueEntries(ctx context.Context) ([]string, error) {
	entries, err := c.GetAllQueueEntries(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var cleanedIDs []string

	for _, entry := range entries {
		// Check if heartbeat has expired
		if now.Sub(entry.LastHeartbeat.ToTime()) > types.QueueHeartbeatTimeout {
			cleanedIDs = append(cleanedIDs, entry.ID)

			// Release partial allocations if any
			if len(entry.AllocatedGPUs) > 0 {
				for _, gpuID := range entry.AllocatedGPUs {
					// Clear the GPU state (release the partial allocation)
					availableState := &types.GPUState{
						LastReleased: types.FlexibleTime{Time: now},
					}
					if err := c.SetGPUState(ctx, gpuID, availableState); err != nil {
						fmt.Printf("Warning: failed to release partial allocation for GPU %d: %v\n", gpuID, err)
					}
				}
			}

			// Remove from queue
			if err := c.RemoveFromQueue(ctx, entry.ID); err != nil {
				fmt.Printf("Warning: failed to remove stale queue entry %s: %v\n", entry.ID, err)
			}
		}
	}

	return cleanedIDs, nil
}

// GetQueueStatus returns the current queue status for display
func (c *Client) GetQueueStatus(ctx context.Context) (*types.QueueStatus, error) {
	entries, err := c.GetAllQueueEntries(ctx)
	if err != nil {
		return nil, err
	}

	status := &types.QueueStatus{
		Entries:      entries,
		TotalWaiting: len(entries),
	}

	for _, entry := range entries {
		status.TotalGPUsRequested += entry.GetRequestedGPUCount()
		status.TotalGPUsAllocated += len(entry.AllocatedGPUs)
	}

	return status, nil
}
