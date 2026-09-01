# docker-offline-kit

[![test](https://github.com/xuxusheng/docker-offline-kit/actions/workflows/test.yml/badge.svg)](https://github.com/xuxusheng/docker-offline-kit/actions/workflows/test.yml)

**Docker + Docker Compose 一键离线安装器**——为网络受限的服务器而生：在有网的机器上打一个包，拷到任何 Linux 服务器上，一条命令装好 Docker + Compose，装完立即可部署项目。

## 🚀 快速开始

```bash
# 1. 下载（国内服务器用加速前缀，见下文「下载」）
curl -fL -o docker-offline-installer.run \
  "https://ghfast.top/https://github.com/xuxusheng/docker-offline-kit/releases/download/v1.1/docker-offline-installer-29.7.2%2B5.5.0-universal.run"

# 2. 安装（自动预检 → 提权 → 安装 → 启动 → 自验）
sudo bash docker-offline-installer.run

# 3. 部署你的项目
docker compose up -d
```

无需 root 登录（sudo 即可）、无需外网、无需任何系统依赖。装完非 root 用户执行 `sudo usermod -aG docker $USER` 并重新登录即可免 sudo 使用。

## 📥 下载

从 [Releases](https://github.com/xuxusheng/docker-offline-kit/releases) 下载安装器：

| 文件 | 适用 |
|---|---|
| `docker-offline-installer-*-universal.run` | **通用**（x86_64/aarch64/armhf 自动选择），推荐 |
| `installer-x86_64` / `installer-aarch64` / `installer-armhf` | 单架构瘦身版（约为通用包 1/3 体积） |

按目标机网络环境选择下载方式：

**① 能直连 GitHub**（境外服务器）：

```bash
wget https://github.com/xuxusheng/docker-offline-kit/releases/download/v1.1/docker-offline-installer-29.7.2%2B5.5.0-universal.run
```

**② 国内服务器**（直连实测仅 ~20KB/s）：在原始 URL 前加加速代理前缀，实测可提速到 1.8~2.4MB/s：

```bash
# 首选
curl -fL -o docker-offline-installer.run \
  "https://ghfast.top/https://github.com/xuxusheng/docker-offline-kit/releases/download/v1.1/docker-offline-installer-29.7.2%2B5.5.0-universal.run"

# 备选
curl -fL -o docker-offline-installer.run \
  "https://gh-proxy.com/https://github.com/xuxusheng/docker-offline-kit/releases/download/v1.1/docker-offline-installer-29.7.2%2B5.5.0-universal.run"
```

**③ 完全离线的目标机**：在任何有网机器上按 ①② 下载，再 `scp` 或 U 盘拷贝过去。

> 💡 **规则**：加速下载 = `加速域名 + 完整的 GitHub 原始 URL`。公益加速域名时效性强，若失效可搜索"GitHub 加速代理"获取当下可用域名，套同一规则。

## 🛠 用法与常用参数

```bash
sudo bash docker-offline-installer-*.run          # 标准安装
sudo bash docker-offline-installer-*.run --yes    # 所有确认取默认（全自动）
sudo bash docker-offline-installer-*.run --no-systemd   # 无 systemd 环境（nohup 兜底）
sudo bash docker-offline-installer-*.run --mirror <加速地址>   # 写入 registry-mirrors
docker-offline-installer --help                   # 全部参数
```

只想体检不安装？`doctor` 子命令单独跑预检（架构/内核/overlay2/cgroup/iptables/systemd/旧版来源/磁盘，共 11 项）：

```bash
bash docker-offline-installer-*.run doctor
```

安装流程自动完成：**环境预检 → 提权（root / sudo / su 自动降级）→ 安装引擎与 Compose → systemd 注册（或 nohup 兜底）→ 启动 → 自验**。

## ❓ 常见问题

**Q：目标机已装过 Docker，能直接覆盖吗？**

能。覆盖前自动备份旧版二进制（`/usr/local/bin/.dok-backup-<时间戳>`），升级失败会**自动回滚**到旧版并恢复服务；`/var/lib/docker` 里的镜像和容器数据全程不动。若旧版是通过 apt/yum 装的，doctor 会额外警告双版本共存风险——这种情况建议继续用包管理器升级。

**Q：装失败了怎么排查？**

全程日志自动写入 `/tmp/dok-install-<时间戳>.log`，失败时终端会显示日志路径。已装 Docker 升级失败的，回滚机制会自动恢复旧版。

**Q：文件传输过程中损坏了怎么办？**

不用手动校验——安装器内嵌 sha256 自校验，文件损坏会在启动时当场报错（显示期望/实际哈希）。也可用 Release 页附带的 `.sha256` 文件手动核对：`sha256sum -c *.sha256`。

**Q：没有 systemd 的老系统能装吗？**

能，加 `--no-systemd`，改用 nohup 方式启动（无开机自启，安装器会提示如何补 rc.local）。

**Q：没有 root 权限呢？**

sudo（含密码）或 su 均可，安装器自动探测降级；docker 用户组会自动创建。

## 📦 自行打包（联网机）

```bash
make pack                                 # 全架构 + universal + sha256，产物在 release/
go run ./cmd/pack --archs x86_64,aarch64  # 按需选架构
# 版本: --docker-version / --compose-version
```

## ✅ 测试覆盖

CI 矩阵（见 [`.github/workflows/test.yml`](.github/workflows/test.yml)）：

- 提权三路径：root 直装（真 systemd）/ sudo 带密码 / su 兜底
- 发行版容器档：Debian 12 / Ubuntu 20.04 / AlmaLinux 8 / **openEuler 24.03**
- 架构：x86_64、**aarch64 原生 runner**（真硬件）、armv7（qemu-user 档）
- 幂等覆盖安装：已有 Docker 时数据保留

## 📐 设计说明

- **官方静态二进制**：对 glibc 零依赖，一个包通吃 Ubuntu/Debian/CentOS 系/国产发行版（[download.docker.com](https://download.docker.com/linux/static/stable/)）；
- **单文件交付**：universal 包 = bash 引导壳 + 内嵌多架构 Go 静态安装器，目标机零依赖，自带 sha256 自校验；
- **已知取舍**：静态二进制无自动安全更新（官方建议生产环境用包管理器安装，本工具定位为离线/受限网络场景）——跟进安全版本请用新版重新打包。

## License

[MIT](LICENSE)
