# 用户快速上手

这份指南给所有在共享 GPU 机器上使用 canhazgpu 的同事。工具已经配置好，你只需要学会下面几个命令。

## 1. 先看看现在有什么

```bash
canhazgpu status
canhazgpu status -v    # 显示进程名（最多两个）
canhazgpu status -vv   # 显示所有进程
```

`status` 默认还会在下面附带打印今天的 schedule 排期；只想看状态可以用 `canhazgpu status --no-schedule`。

## 2. 跑一个任务（推荐）

```bash
canhazgpu run --gpus 1 -- python train.py
```

`run` 会自动预约 GPU、设置 `CUDA_VISIBLE_DEVICES`、跑完自动释放。注意 `--` 必须写，它分隔 canhazgpu 的参数和你的命令。

常用选项：

```bash
# 多卡训练
canhazgpu run --gpus 2 -- python -m torch.distributed.launch --nproc_per_node=2 train.py

# 指定某几张卡
canhazgpu run --gpu-ids 2,3 -- python train.py

# 防止失控任务：最多跑 12 小时
canhazgpu run --gpus 2 --timeout 12h -- python train.py

# 没卡时最多等 30 分钟
canhazgpu run --gpus 2 --wait 30m -- python train.py

# 没卡就立刻失败（适合脚本）
canhazgpu run --gpus 1 --nonblock -- python train.py

# 给任务加个说明，别人 status 时能看到
canhazgpu run --gpus 1 --note "finetune-bert" -- python train.py
```

GPU 忙的时候 `run` 会自动排队，排队情况用 `canhazgpu queue` 查看。

## 3. 交互式使用（notebook、调试、多步实验）

```bash
# 预约 1 张 GPU 4 小时，并把卡号写进环境变量
export CUDA_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 1 --duration 4h --short)

jupyter notebook
```

用完立刻释放：

```bash
canhazgpu release                 # 释放你所有的 manual 预约
canhazgpu release --gpu-ids 0,2   # 只释放某几张卡
```

几个小知识：

- 如果忘了先预约、自己的进程已经跑在某张卡上，可以补一个预约；只有你自己的进程占着这张卡时会立即成功，还有别人的进程时会自动排队：

```bash
canhazgpu reserve --gpu-ids 6 --claim --duration 2h
```

- 预约时长要显式写，默认只有 30 分钟
- 连续 15 分钟没有使用会自动释放，防止忘记
- `release` 不带参数会释放你的全部 manual 预约，脚本里建议用 `--gpu-ids`

## 4. 预约未来时段

```bash
canhazgpu schedule                       # 查看今天的排期
canhazgpu schedule --date tomorrow       # 查看明天

# 预约今天 14:00-16:00 的 2 张卡
canhazgpu reserve --start 14:00 --end 16:00 --gpus 2 --note "demo"

# 取消预约
canhazgpu schedule --cancel 4f89853e
```

预约到点后会自动占用你选的卡；如果之前有人占着，他们的预约会被释放（进程不会被杀掉），所以约别人的卡之前先打个招呼。

## 5. 查看自己的使用历史

```bash
canhazgpu report --days 7
```

## 6. 收到警告怎么办

如果你没预约就用了 GPU，可能会在终端收到 canhazgpu 的提醒。不用慌：

- 先 `canhazgpu status` 看看哪些卡空闲
- 用 `canhazgpu run` 或 `canhazgpu reserve` 正常预约后再继续
- 想了解情况可以看 `canhazgpu violations`

## 7. 几条好习惯

1. 开始任务前先 `canhazgpu status`
2. 长任务用 `run` + `--timeout`，不要裸跑
3. 用完马上 `release`
4. 不要绕过预约直接用 GPU，会占用别人等着的资源
5. 加 `--note`，让同事知道这张卡在跑什么

## 8. 常见问题

| 现象 | 怎么办 |
|---|---|
| `Not enough GPUs available` | `canhazgpu status` / `canhazgpu queue`，等一会或减少请求 |
| 命令报错退出 | GPU 会自动释放，检查命令本身即可 |
| 忘了自己约了哪张卡 | `canhazgpu status` 看 USER 列 |
| 自己的卡被别人用了 | `canhazgpu status` 会标注 FOREIGN，找对方协调 |

## 详细文档

- [中文快速上手](quickstart-zh.md)
- [run 详细用法](usage-run.md)
- [reserve 详细用法](usage-reserve.md)
- [schedule 预约详解](usage-schedule.md)
