# docker-offline-kit

[![test](https://github.com/xuxusheng/docker-offline-kit/actions/workflows/test.yml/badge.svg)](https://github.com/xuxusheng/docker-offline-kit/actions/workflows/test.yml)

**Docker + Docker Compose 一键离线安装器**：在联网机器上打一个包，拷到任何 Linux 服务器上，一条命令装好 Docker + Compose，装完立即可部署项目。

## 下载

从 [Releases](https://github.com/xuxusheng/docker-offline-kit/releases) 下载（或离线拷贝）安装器：

| 文件 | 适用 |
|---|---|
| `docker-offline-installer-*-universal.run` | **通用**（x86_64/aarch64/armhf 自动选择），推荐 |
| `installer-x86_64` / `installer-aarch64` / `installer-armhf` | 单架构瘦身版（约为通用包 1/3 体积） |

## 用法

```bash
sudo bash docker-offline-installer-*.run        # 一条命令，完事
```

安装器自动完成：**环境预检 → 提权（root / sudo / su 自动降级）→ 安装引擎与 Compose → systemd 注册（或 nohup 兜底）→ 启动 → 自验**。装完即可 `docker compose up` 部署项目。

常用参数：

```bash
sudo bash installer.run --yes              # 所有确认取默认（全自动）
sudo bash installer.run --no-systemd       # 无 systemd 环境（nohup 兜底）
sudo bash installer.run --mirror <加速地址>  # 写入 registry-mirrors
installer.run --help                       # 全部参数
```

## 自行打包（联网机）

```bash
make pack                    # 全架构 + universal + sha256，产物在 release/
go run ./cmd/pack --archs x86_64,aarch64   # 按需选架构
# 版本: --docker-version / --compose-version
```

## 测试覆盖

CI 矩阵（见 [`.github/workflows/test.yml`](.github/workflows/test.yml)）：

- 提权三路径：root 直装（真 systemd）/ sudo 带密码 / su 兜底
- 发行版容器档：Debian 12 / Ubuntu 20.04 / AlmaLinux 8 / **openEuler 24.03**
- 架构：x86_64、**aarch64 原生 runner**（真硬件）、armv7（qemu-user 档）
- 幂等覆盖安装：已有 Docker 时数据保留

环境预检（`doctor`）覆盖：架构、内核 ≥3.10、overlay2、cgroup、iptables、xz、systemd、旧版残留、磁盘空间。

## 设计说明

- **官方静态二进制**：对 glibc 零依赖，一个包通吃 Ubuntu/Debian/CentOS 系/国产发行版（[download.docker.com](https://download.docker.com/linux/static/stable/)）；
- **单文件交付**：universal 包是 bash 引导壳 + 内嵌多架构 Go 静态安装器，目标机零依赖；
- **已知取舍**：静态二进制无自动安全更新（官方建议生产用包管理器安装，本工具定位为离线/受限网络场景）——跟进安全版本请用新版重新打包。

## License

[MIT](LICENSE)
