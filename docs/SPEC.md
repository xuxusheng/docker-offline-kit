# docker-offline-kit 需求规格与架构（v1 定稿 2026-09-01）

## 目标

在有网的机器上打一个包，拿到任何 Linux 服务器上一条命令装好 Docker + Docker Compose，装完立即可部署项目。

## 需求条目

| # | 需求 | 定案 |
|---|---|---|
| R1 | 架构 | x86_64 + aarch64 实测；armv7 按业界标准用 qemu-user 容器档实测（全系统模拟经实证不可行，见 R1 注） |
  > R1 注（2026-09-01 实证）：Debian 12 官方已停止发布 armhf 云镜像；自建 rootfs 全系统模拟在 armmp 内核下 virtio 块设备无法挂载（/dev/vda1 不存在，initramfs virtio 模块齐全仍失败），TCG 全系统路线放弃。armv7 与 aarch64 共享同一份 Go 安装逻辑，仅指针宽度差异，运行时行为由 aarch64 真机实测背书。 |
| R2 | 发行版 | 静态二进制实现 glibc 免疫；测试覆盖 Ubuntu/Debian/CentOS/openEuler 等 |
| R3 | 提权 | root 直接 → sudo（NOPASSWD / 带密码交互 / --sudo-pass 自动化）→ su 兜底；rootless 二期 |
| R4 | 预检 doctor（强化版） | 身份路径判定 + 架构/内核≥3.10/overlay2/cgroup/iptables≥1.4/systemd/磁盘/旧版残留；红黄绿报告，红灯阻断 |
| R5 | 无 systemd 兜底 | nohup 拉起 dockerd + rc.local 提示（官方静态安装文档路径） |
| R6 | 幂等 | 覆盖升级保留 /var/lib/docker |
| R7 | 交付形态 | **单文件 universal**：bash 引导壳内嵌多架构 Go 静态安装器（CGO_ENABLED=0），uname -m 自动选路；附带 sha256 |
| R8 | 实现语言 | Go（安装器 + pack）；引导壳为极小 bash |
| R9 | 可选参数 | DOCKER_VERSION / COMPOSE_VERSION / REGISTRY_MIRROR（默认最新版） |
| R10 | 实测 | 容器档（多发行版含 openEuler，privileged+systemd）+ QEMU VM 档（发版前）+ 真机：内部 x86_64 Ubuntu 20.04 服务器、内部 aarch64 Debian 11 测试机（覆盖安装幂等测试） |
| R11 | 本期不做 | Windows/macOS；rootless（二期第一顺位）；deploy SSH 直推子命令（接口预留） |
| R12 | 交互设计（UX） | 交互式但克制：彩色分阶段进度（✓/⚠/●/○ + 步骤进度条）+ 三类决策点交互（覆盖升级确认 / sudo 密码 / 失败恢复三选一）+ 默认值回车到底；`--yes`/`--non-interactive`/`--sudo-pass` 保留全自动通道。逻辑与显示分离，非交互模式输出纯文本日志 |

## 依赖选型（全部 vendor 入库，纯 Go 零 cgo）

| 依赖 | 用途 |
|---|---|
| spf13/cobra | CLI 子命令/flag/帮助 |
| fatih/color | ANSI 颜色，TTY 自动降级 |
| golang.org/x/term | 密码输入关回显（替代 huh/bubbletea 重型 TUI——线性安装流不需要全屏 TUI，最小依赖原则） |
| schollz/progressbar/v3 | 步骤/字节进度条 |

构建细节：`GOPROXY=https://goproxy.cn,direct`（国内网络）；`go mod vendor` 构建离线可复现。

## 已知 tradeoff（用户知情确认）

- 静态二进制无自动安全更新（官方文档明示，官方推荐生产用包管理器）。本工具定位 = 离线/受限网络场景；跟进安全更新 = 用新版重打包。

## 架构

```
打包机（有网）                          目标机（离线，零依赖）
─────────────────                      ─────────────────────────
pack (Go)                              installer (Go, 静态 ELF)
 1. 下载官方静态包(各架构)               引导壳(bash) → uname -m 选架构
 2. 下载 compose 插件(各架构)            ① doctor 预检，红灯即停
 3. embed 注入 payload                  ② 提权判定 root→sudo→su
 4. CGO_ENABLED=0 交叉编译               ③ 解包 → 装二进制+compose
 5. bash 引导壳拼接 → universal .run    ④ systemd unit / nohup 兜底
 6. 产出 sha256                         ⑤ 幂等（保数据）⑥ 自验
```

## 代码结构

```
cmd/pack/main.go        打包侧（联网机）
cmd/installer/main.go   安装侧（交叉编译，payload embed）
internal/doctor/        预检
internal/privilege/     提权三级降级
internal/installer/     安装/幂等/systemd|nohup
internal/arch/          架构映射
scripts/bootstrap.sh    universal 引导壳模板
scripts/test/           容器档 + QEMU 档测试流水线
```

## 关键技术事实（调研确认）

- 官方静态二进制前置条件：64 位、内核≥3.10、iptables≥1.4、xz utils、正确挂载的 cgroupfs（docs.docker.com/engine/install/binaries/）
- Rootless 前提（二期）：uidmap 包、/etc/subuid|subgid ≥65536 条目、rootless-extras 包、dockerd-rootless-setuptool.sh
- Compose 插件 = GitHub releases 单文件，放 cli-plugins 目录
- Go 交叉编译零配置：GOARCH=arm GOARM=7；唯一常见坑：必须 CGO_ENABLED=0
- QEMU：armv7 用 qemu-system-arm + Debian armhf 云镜像（全系统模拟）；发行版矩阵用容器档（privileged+systemd 镜像）+ KVM 云镜像档

## 版本基线（2026-09-01）

- Docker Engine 29.7.2（download.docker.com 最新稳定）
- Compose v5.5.0
