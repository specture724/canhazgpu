# 宿主机守护与 Docker 开发环境

在裸金属宿主机运行一个 root 权限的 `canhazgpu guard`，开发者在自己的
Docker 中照常使用 `run`、`reserve`、`release`、`queue`、`cancel`、`status`。
容器可以使用 root 账户及独立的 PID、网络命名空间。

宿主机通过 Unix socket 的内核 peer credentials 获取调用进程的**宿主机 PID**，
从宿主机 cgroup 找到 Docker 容器，再读取 `canhazgpu.owner` 标签或管理员的归属配置。
因此两个 root 容器仍属于两个不同用户。任务及 supervisor 在容器内执行，
Redis 中记录宿主机 PID；查询设备及取消任务由宿主机执行。
`status` 中的进程 PID 始终使用宿主机编号，内外一致。

## 1. 宿主机设置

安装同一版本的二进制，并使用现有 Redis 及设备池。尚未初始化时才执行 `admin`：

```bash
canhazgpu admin --gpus 8 --provider nvidia
sudo canhazgpu guard --listen /run/canhazgpu/host.sock --enforce
```

选择机器实际支持的 provider。在含 Ascend 支持的分支上使用 `--provider ascend`。
不要用 `admin --force` 覆盖正在使用的设备池。已有 guard 时先停止原实例；
同一个 Redis 数据库仍只允许一个 guard。

开机启动可使用仓库中的 `deploy/canhazgpu-docker-guard.service`，按实际情况
设置 Redis 参数及 guard 策略。它使用 `RuntimeDirectory` 管理 socket 目录。
如已有 guard 服务，只需为其增加 `--listen /run/canhazgpu/host.sock`，并加入
`RuntimeDirectory=canhazgpu` 和 `RuntimeDirectoryMode=0755`。

宿主机与容器客户端都会自动发现 `/run/canhazgpu/host.sock`。
其他路径用 `--host-socket PATH` 或 `CANHAZGPU_HOST_SOCKET=PATH` 指定。
`--host-socket off` 可显式使用本机直接连接模式。
配置了 socket 后，连接失败会直接报错，不会转而使用容器的 root 身份、本地 PID 或本地 Redis。

## 2. 创建开发容器

在自己的宿主机账户下创建容器，把账户写入标签，挂载 **socket 所在目录**及二进制：

```bash
docker run -it --name my-dev \
  --label "canhazgpu.owner=$(id -un)" \
  -e CANHAZGPU_HOST_SOCKET=/run/canhazgpu/host.sock \
  --mount type=bind,src=/run/canhazgpu,dst=/run/canhazgpu,readonly \
  --mount type=bind,src=/usr/local/bin/canhazgpu,dst=/usr/local/bin/canhazgpu,readonly \
  --gpus all \
  YOUR_DEVELOPMENT_IMAGE bash
```

这里的 `--gpus all` 适用于 NVIDIA Container Toolkit。AMD/Ascend 请保留开发环境
原有的设备和驱动挂载。**容器必须暴露整个共享设备池，并保持与宿主机相同的设备编号**；
目前不转换仅暴露部分设备或重排设备后的编号。Ascend 通常需要所有 `/dev/davinciN`、
`davinci_manager`、`devmm_svm`、`hisi_hdc` 和对应的宿主机驱动目录。
canhazgpu 设置运行时可见设备变量，不代替 Docker 的设备映射。

Redis 连接通过同一个 socket 转发到守护进程使用的 Redis，数据库编号和内存阈值
也从守护进程获取；无需容器联网或暴露 Redis TCP 端口，也无需挂载 Docker socket
或宿主机 `/proc`，无需 `--pid=host`。
挂载整个 socket 目录可让客户端在守护进程重启后找到新 socket。

如果使用 `sudo docker run`，请在 sudo 前取得真实账户：

```bash
owner=$(id -un)
sudo docker run --label "canhazgpu.owner=$owner" ...
```

标签必须是存在的非 root 宿主机账户。标签声明的是容器归属，不是容器内 `$USER`。
Docker 的创建者账户不能从容器内 root 自动推断。

## 3. 已有容器无需重建

管理员可创建 JSON 文件，将**完整的容器 ID**映射为宿主机账户：

```bash
docker inspect --format '{{.Id}}' existing-dev
```

例如 `/etc/canhazgpu/docker-owners.json`：

```json
{
  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": "alice"
}
```

```bash
sudo canhazgpu guard --listen /run/canhazgpu/host.sock \
  --docker-owners /etc/canhazgpu/docker-owners.json --enforce
```

映射优先于标签，启动时加载；修改后重启 guard。它也适用于宿主机默认 Docker
daemon 无法 inspect 的 rootless 容器。已有容器仍须有 socket 目录和二进制挂载；
Docker 不能向运行中的容器补充普通 bind mount，缺少挂载时需按原配置重建。
无法确认归属的容器不会被默认为宿主机 root，客户端会提示补齐配置；
设备扫描会显示 `unknown`，guard 不会终止身份不明的进程。

`/etc/canhazgpu/docker-owners.json` 会被宿主机所有命令自动读取。
因此即使 guard 停止，直接执行 `canhazgpu status` 也能显示映射中的账户；
不需要为了查看归属而启动强制执行。其他路径可使用全局 `--docker-owners PATH`
或 `CANHAZGPU_DOCKER_OWNERS=PATH`。独立 status 每次调用读取文件，guard 修改映射后需重启。

## 4. 容器内外使用

```bash
canhazgpu run --gpus 1 -- python train.py
canhazgpu run --gpu-ids 0 --idle-timeout 0 -- python
canhazgpu status
canhazgpu status --json
canhazgpu queue
canhazgpu cancel TASK_ID
canhazgpu reserve --gpus 1 --duration 2h
canhazgpu release
```

终端、标准输入输出、命令退出码和 Ctrl-C 由本地任务保留；supervisor 使用容器内 PID
监控任务，并通过 bridge 维持预约和执行空闲检查。容器内外均按宿主机账户进行排队、
预约归属、使用归属及取消检查。`--user` 仍可设置显示名，实际归属不会改变。
容器 root 不具有宿主机管理员身份，不能通过 `cancel --force` 取消其他用户的任务。
需要管理员取消时，在宿主机使用 `sudo canhazgpu cancel TASK_ID --force`。

guard 的违规检测、宽限时间、警告和 SIGINT → SIGTERM → SIGKILL 策略保持一致。
进程警告可经宿主机 `/proc/<pid>/fd/2` 到达容器终端。取消容器任务时宿主机同时处理
主进程和该任务设备上属于该用户的设备进程。若任务直接作为容器 PID 1 运行，建议为镜像
配置 init（Docker 安装了 docker-init 时可用 `--init`），以便正常处理信号及回收子进程。

## 信任范围与运维

这仍是共享开发机上的协作调度系统：获准连接 socket 的客户端能访问同一 Redis，
标签/映射用于记账，不是隔离恶意 Docker 管理员的安全边界。
socket 默认允许本机用户连接；目录应由宿主机管理员控制。只给受信开发容器挂载它。
需要限制客户端时可通过目录访问权限控制。

守护正常退出会移除 socket；若被 SIGKILL 后残留 socket，手工启动会拒绝覆盖，
应确认没有正在使用它的守护后清理。推荐 systemd 的 RuntimeDirectory 自动管理生命周期。
容器与宿主机应使用同一版 canhazgpu。

## 验证

```bash
CGO_ENABLED=0 go build -o build/canhazgpu-docker .
python3 scripts/test-docker.py --image YOUR_LOCAL_IMAGE
```

集成脚本需要普通宿主机账户、可访问的 Docker、免密 sudo、Redis 及含 `sh`/`sleep`
的本地镜像。它创建三个真实 root 容器（标签归属当前账户与 `nobody`，以及一个通过配置归属的容器）、独立 Redis、
临时 root 守护和模拟设备查询。它验证归属、宿主机 PID、状态一致、内外取消、
跨用户拒绝、归属映射、守护重启、排队取消、设备变量、退出码、自动释放、失联报错，以及 guard 对真实
容器进程的终止。模拟设备不会使用真实加速卡；结束会清理测试资源。

实现依据：[Docker labels](https://docs.docker.com/engine/manage-resources/labels/)、
[Docker process isolation](https://docs.docker.com/engine/containers/run/)、
[Linux Unix socket peer credentials](https://man7.org/linux/man-pages/man7/unix.7.html)。

### 本机验证记录（2026-09-06）

- Go 完整测试（含独立 Redis 集成测试）及 race 检查通过，`go vet ./...` 通过。
- Docker 18.09、Linux 5.10、arm64：无网络、独立 PID、非 privileged 的真实 root
  容器通过上述模拟设备集成测试。
- 在 NPU 分支上使用真实 Ascend 910B1、驱动 25.5.0 和本机
  `quay.io/ascend/vllm-omni:v0.28.0` 镜像验证：先预约物理卡 7，容器内
  `canhazgpu run --gpu-ids 7` 运行 torch_npu 张量任务；实际设备进程占用 130 MB，
  内外状态均归属 `ajhou`，宿主机 PID 一致，容器内取消后进程和预约均释放。
  此机器的驱动需要 privileged 容器才能成功初始化；该验证仍使用独立 PID 和无网络模式。
  这属于设备运行环境要求，bridge 本身已在非 privileged 容器中通过验证。
