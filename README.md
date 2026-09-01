# docker-offline-kit

Docker + Docker Compose 一键离线安装工具：在有网的机器上打包，拿到任何 Linux 服务器上一条命令装好，装完立即可用。

## 现状

- **v0（当前）**：bash 实现，`docker-offline-kit.sh`，支持 pack / doctor / install / verify，双架构（x86_64/aarch64）。
- **v1（进行中）**：Go 重写——静态单文件安装器 + universal bash 引导壳，三级提权（root→sudo→su），强化 doctor，QEMU/容器多发行版测试流水线。规格见 [docs/SPEC.md](docs/SPEC.md)。

## v0 快速用法

```bash
# 联网机打包（--multi 双架构合一包）
./docker-offline-kit.sh pack --multi

# 目标机预检
./docker-offline-kit.sh doctor

# 目标机安装（tar 解压后）
sudo ./install.sh            # 无 systemd 环境加 --no-systemd

# 验证
./docker-offline-kit.sh verify
```

可选参数：`DOCKER_VERSION` / `COMPOSE_VERSION`（自定义版本）、`ARCH`（单架构打包）、`REGISTRY_MIRROR`（镜像加速，写进目标机 daemon.json）。

## 设计要点

- 官方静态二进制（[download.docker.com](https://download.docker.com/linux/static/stable/)）→ 跨发行版、glibc 免疫；静态包无自动安全更新，需定期重打包跟进（官方建议生产用包管理器，本工具定位为离线/受限网络场景）。
- Compose 插件为 GitHub releases 单文件，放 cli-plugins 目录即可。
- 覆盖安装幂等：保留 `/var/lib/docker` 数据。
