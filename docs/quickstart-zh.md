# 团队快速上手（中文版）

这份指南面向所有在共享 GPU 主机上使用 canhazgpu 的同事，覆盖日常最常用的命令：`status`、`run`、`reserve`、`schedule`、`queue`、`release`，以及让整个团队用得舒心的习惯。

## canhazgpu 是什么

canhazgpu 是一套面向单台共享主机的协作式 GPU 预约系统：

- 每张 GPU 在 Redis 里都有一条 **reservation**（run 型、manual 型，或未来的 booking）。
- `run` 把 reservation 和进程绑定，进程结束自动释放。
- `reserve` 为交互式工作提供有时长的 reservation。
- `schedule` 展示未来的预约，方便提前规划。
- `guard`（通常以服务方式运行）会警告没有预约就使用 GPU 的人，也可以执行强制策略。

## 0. 首次初始化（仅管理员）

如果主机已经初始化过，跳过本节。一次性初始化：

```bash
# NVIDIA 主机
canhazgpu admin --gpus $(nvidia-smi -L | wc -l)

# AMD 主机
canhazgpu admin --gpus $(amd-smi list --json | jq 'length')

# 华为 Ascend 主机（示例：8 张逻辑 NPU）
canhazgpu admin --gpus 8 --provider ascend
```

不要在繁忙的主机上不带 `--force` 运行它；它会清空所有 reservation。

## 1. 先看再动手：status

```bash
canhazgpu status
```

```
GPU  STATUS      USER     DURATION    TYPE    DETAILS                  MEMORY          NOTE  UTIL
---  ------      ----     --------    ----    -------                  ----------      ----  ----
0    AVAILABLE   -        -           -       free for 1h 2m 30s       1MB used        -     0%
1    IN_USE      alice    0h 15m 30s  RUN     heartbeat 5s ago, processes: PID 123 (2h3m)  8452MB, 1 processes   -     87%
2    IN_USE      bob      0h 30m 0s   MANUAL  expires in 3h 30m 0s     no usage detected  -   5%
3    UNRESERVED  carol    -           -       used by PID 123 (2h3m)  2048MB, 1 processes  -  42%
```

- `AVAILABLE` — 空闲，可以预约。
- `IN_USE` 且 TYPE 为 `RUN` — 有人在跑任务，进程结束即释放。
- `IN_USE` 且 TYPE 为 `MANUAL` — 有人预约了一段时间。
- `UNRESERVED` — 有人没预约就在用 GPU；这张卡会被排除出分配池，`guard` 也可能会介入。
- `⚠ FOREIGN` — GPU 有预约，但上面的进程属于别人。

默认情况下 DETAILS 只显示 PID 和进程已运行时长。加 `-v` 会显示进程名（最多两个），`-vv` 显示全部：

```bash
canhazgpu status -v
canhazgpu status -vv
```

脚本用的机器可读格式：

```bash
canhazgpu status --json
canhazgpu status --json | jq -r '.[] | select(.status == "AVAILABLE") | .gpu_id'
```

## 2. 跑任务：run

`run` 是启动任何加速器负载的首选方式：它负责预约设备、为 NVIDIA/AMD 设置 `CUDA_VISIBLE_DEVICES` 或为 Ascend 设置 `ASCEND_RT_VISIBLE_DEVICES`、运行你的命令，并在命令退出后自动释放。一定要用 `--` 分隔 canhazgpu 的参数和你的命令：

```bash
canhazgpu run --gpus 1 -- python train.py
canhazgpu run --gpus 2 -- python -m torch.distributed.launch --nproc_per_node=2 train.py
canhazgpu run --gpu-ids 2,3 -- python train.py
canhazgpu run --gpus 1 -- python            # 交互式 REPL 也支持
```

常用选项：

| 选项 | 含义 |
|---|---|
| `--gpus N` | 需要的 GPU 数量（默认 1） |
| `--gpu-ids 1,3` | 指定 GPU；会一直等到这些卡空出来 |
| `--timeout 4h` | 超过 4 小时自动结束任务（先 SIGINT，再 SIGKILL） |
| `--wait 1h` | 最多排队等 1 小时，等不到就失败 |
| `--nonblock` | 没有 GPU 就立即失败，不排队 |
| `--note "..."` | 给 reservation 加说明，会显示在 `status` 里 |

GPU 繁忙时，`run` 默认按先来先服务（FCFS）排队并打印进度。排队期间按 Ctrl+C 会把自己的排队条目移除。

```bash
canhazgpu queue                # 查看排队情况
canhazgpu run --wait 30m --gpus 2 -- python train.py
```

长任务推荐写法：

```bash
canhazgpu run --gpus 2 --timeout 12h --note "bert-finetune" -- \
  python train.py --epochs 100 --save-every 10
```

## 3. 交互式工作：reserve

当你不只跑一条命令、而是需要一段时间的设备（notebook、调试、多步骤实验）时，用 `reserve`。它是 **manual** reservation：有时长、不会自动设置 provider 对应的可见设备变量，直到过期、闲置被回收或你手动释放。

```bash
# 1 张 GPU，4 小时
canhazgpu reserve --gpus 1 --duration 4h

# 指定 GPU
canhazgpu reserve --gpu-ids 0,2 --duration 2h

# 预约并一步设置环境变量
export CUDA_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 2 --duration 3h --short)

# 华为 Ascend
export ASCEND_RT_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 2 --duration 3h --short)

jupyter notebook
```

用完尽快释放：

```bash
canhazgpu release                 # 释放当前用户所有 manual reservation
canhazgpu release --gpu-ids 0,2   # 只释放指定 GPU（对 run 型 reservation 也有效）
```

注意事项：

- 默认时长只有 30 分钟——请总是显式传 `--duration`。
- 默认情况下，manual reservation 连续 **15 分钟没有 GPU 使用** 就会被自动释放（`--idle-timeout`，传 `0` 可关闭，最大 3 小时）。这是防止有人忘记释放而浪费资源。如果你要加载大模型再开始训练，可以放宽：`--idle-timeout 1h`。
- `release` 不带 `--gpu-ids` 会释放你**所有** manual reservation，脚本里请用 `--gpu-ids` 精确释放。

## 4. 提前规划：schedule

用 `reserve --start` 像订会议室一样预约未来的时间段，再用 `schedule` 查看：

```bash
canhazgpu schedule                       # 今天的排期
canhazgpu schedule --date tomorrow       # 明天
canhazgpu schedule --days 5              # 接下来 5 天

# 今天 14:00-16:00，2 张 GPU
canhazgpu reserve --start 14:00 --end 16:00 --gpus 2 --note "demo"

# 明天 09:30 开始，持续 4 小时，8 张 GPU
canhazgpu reserve --start 'tomorrow 09:30' --duration 4h --gpus 8

# 取消自己的预约
canhazgpu schedule --cancel 4f89853e
```

booking 的行为：

- 预约时就会选定 GPU，并显示在 `schedule` 里。
- booking 生效前，临时 `run`/`reserve` 会**避开**这些 GPU（`run` 默认避开未来 30 分钟内被预约的卡）。
- 到点后，booking 会**释放当前占用者**。它释放的是 reservation，不是进程——在别人的卡上安排 booking 前，先跟对方打个招呼。
- `--end` 相对 `--start` 解释，所以 `--start 22:00 --end 02:00` 可以跨天。
- 计划有变就早点取消；取消一个已激活的 booking 会立即释放它的 GPU。

## 5. 查看历史：report

```bash
canhazgpu report          # 最近 30 天
canhazgpu report --days 7
```

## 6. 了解守护进程：guard 和 violations

在托管的主机上，`canhazgpu guard` 通常以服务方式运行。它会：

- 警告没有预约就使用 GPU 的人（把警告写进对方的终端）；
- 开启 `--enforce` 时，警告多次后终止进程（SIGINT → SIGTERM → SIGKILL）；
- 定期维护系统：激活到点的 booking、释放过期/闲置的 reservation。

查看它记录的内容：

```bash
canhazgpu violations              # 当前未解决的违规
canhazgpu violations --history    # 已解决的违规（最近 7 天）
```

如果你的任务出现在这里，说明你忘了预约——先别急着和提醒你的进程争论。

## 7. Web 界面：web

```bash
canhazgpu web --port 8080
```

Web 界面部署在服务器上，需要反向代理才能访问。在本地机器上建立 SSH 隧道：

```bash
ssh -L 8080:localhost:8080 L20X-8
```

然后打开 http://localhost:8080 ，可以看到实时状态、排队情况和报表（页面会自动刷新）。

## 决策表

| 你需要什么 | 用什么 |
|---|---|
| 现在跑一条命令 | `canhazgpu run --gpus N -- ...` |
| 交互式会话、notebook、多步骤工作 | `canhazgpu reserve --duration ...` |
| 指定某张 GPU | 加上 `--gpu-ids 0,2` |
| 未来某个时间段的 GPU | `canhazgpu reserve --start ... --end ...` |
| 看谁在排队 | `canhazgpu queue` |
| 看现在谁在用 GPU | `canhazgpu status` |
| 看使用历史 | `canhazgpu report --days N` |
| 看违规记录 | `canhazgpu violations` |
| 释放 manual GPU | `canhazgpu release [--gpu-ids ...]` |

## 最佳实践

### 批量任务：优先用 run

`run` 帮你处理预约、心跳和自动释放，而且 SSH 断开也不受影响（supervisor 会忽略 SIGHUP）。不需要额外包 `nohup`：

```bash
# 推荐
canhazgpu run --gpus 2 --timeout 12h --note "finetune-7b" -- ./train.sh

# 没必要——这里的 nohup 没有任何额外作用
canhazgpu run --gpus 2 -- nohup ./train.sh
```

用 `--timeout` 而不是指望系统帮你杀掉失控的任务。

### manual reservation：短一点、准一点

```bash
# 不推荐：用默认时长，只能等过期或闲置回收
canhazgpu reserve --gpus 1

# 推荐：明确时长和闲置宽限
canhazgpu reserve --gpus 1 --duration 2h --idle-timeout 30m --note "debugging"
```

### 脚本：快速失败，精确清理

```bash
#!/usr/bin/env bash
set -euo pipefail

# 单条命令时，run 全包了：
canhazgpu run --gpus 2 -- python train.py --epochs 10

# 多步骤会话：显式预约，按 GPU ID 释放：
export CUDA_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 2 --duration 3h --short)
trap 'canhazgpu release --gpu-ids "$CUDA_VISIBLE_DEVICES"' EXIT

python preprocess.py
python train.py
python evaluate.py
```

### 自动化：用 JSON，不要解析表格

```bash
# 等到至少一张 GPU 可用再跑任务
while ! canhazgpu status --json | jq -e 'any(.[]; .status == "AVAILABLE")' > /dev/null; do
  sleep 30
done
canhazgpu run --gpus 1 -- python train.py
```

### 团队礼仪

1. 开始任何任务前先 `canhazgpu status`。
2. 一律通过 `run` 或 `reserve` 使用 GPU——未预约使用会显示为 `UNRESERVED`、被排除出分配池，还会触发 guard 警告。
3. 按需预约，不多占，用完早释放。
4. 如果 booking 会抢占别人的 GPU，提前告知。
5. 用 `--note` 让别人知道这条 reservation 是干什么的。

## 个性化默认配置

把常用偏好写进 `~/.canhazgpu.yaml`，省得每次敲一堆参数：

```yaml
memory:
  threshold: 100        # 超过这个 MB 数才认为 GPU 在占用
run:
  timeout: "8h"          # run 任务的默认超时
reserve:
  duration: "2h"         # 你的默认预约时长
  idle-timeout: "30m"
```

## 常见问题排查

| 现象 | 处理 |
|---|---|
| `Not enough GPUs available` | `canhazgpu status`，再 `canhazgpu queue`；等待或减少请求数量 |
| 命令失败了 | GPU 会自动释放；检查命令本身的输出 |
| `failed to connect to Redis` | `redis-cli ping`；确认 Redis 在运行、配置指向正确的主机 |
| 自己的 GPU 被 booking 抢走 | 这是提前安排好的；booking 到点会释放 reservation |
| 被 guard 警告 | 你未预约使用了 GPU；下次先 reserve |

## 延伸阅读

- [命令总览](commands.md)
- [run 详细用法](usage-run.md)
- [reserve 详细用法](usage-reserve.md)
- [schedule 预约详解](usage-schedule.md)
- [guard 与强制策略](features-guard.md)
- [配置说明](configuration.md)
