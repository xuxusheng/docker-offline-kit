#!/usr/bin/env bash
# docker-offline-kit — Docker + Docker Compose 一键离线安装工具（多架构 / 跨发行版）
#
# 用法：
#   联网机打包:   ./docker-offline-kit.sh pack [--multi]        # --multi 双架构(x86_64+aarch64)合一包
#   目标机预检:   ./docker-offline-kit.sh doctor                # 兼容性体检，绿黄红报告
#   目标机安装:   ./docker-offline-kit.sh install [包路径] [--no-systemd]
#   安装后验证:   ./docker-offline-kit.sh verify
#
# 可配置（环境变量）：
#   DOCKER_VERSION=29.7.2  COMPOSE_VERSION=v5.5.0
#   ARCH=x86_64            # pack 单架构时指定
#   REGISTRY_MIRROR=       # 逗号分隔的镜像加速地址，写进目标机 daemon.json
set -euo pipefail

DOCKER_VERSION="${DOCKER_VERSION:-29.7.2}"
COMPOSE_VERSION="${COMPOSE_VERSION:-v5.5.0}"
ARCH="${ARCH:-x86_64}"
BASE="https://download.docker.com/linux/static/stable"
COMPOSE_URL="https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-"
WORKDIR=".docker-offline-build"
KIT_NAME="docker-offline-kit"

die() { echo "[ERROR] $*" >&2; exit 1; }
log() { echo "[docker-offline-kit] $*"; }

# ============ doctor：目标机兼容性预检 ============
cmd_doctor() {
  local RED=0 YEL=0
  ok()   { echo "  [绿] $*"; }
  warn() { echo "  [黄] $*"; YEL=$((YEL+1)); }
  bad()  { echo "  [红] $*"; RED=$((RED+1)); }

  echo "== docker-offline-kit doctor =="

  # 架构
  local m; m=$(uname -m)
  case "$m" in
    x86_64|aarch64|arm64) ok "架构 $m 支持且将自动匹配" ;;
    *) bad "架构 $m 不受支持（仅 x86_64/aarch64）" ;;
  esac

  # 内核
  local kv; kv=$(uname -r | cut -d. -f1,2)
  if awk -v v="$kv" 'BEGIN{exit !(v>=3.10)}'; then ok "内核 $(uname -r) >= 3.10"; else bad "内核 $(uname -r) 过旧 (<3.10)，Docker 无法运行"; fi

  # overlay2
  if modprobe overlay 2>/dev/null || grep -q overlay /proc/filesystems; then ok "overlay2 存储驱动可用"; else bad "overlay 内核模块不可用，需要升级内核或改用 vfs（性能差）"; fi

  # cgroup
  if [ -f /sys/fs/cgroup/cgroup.controllers ]; then ok "cgroup v2"; else ok "cgroup v1"; fi

  # iptables（缺失是最常见的“装好了但容器没网”根因）
  if command -v iptables >/dev/null 2>&1; then
    ok "iptables: $(command -v iptables)"
  elif command -v nft >/dev/null 2>&1; then
    warn "无 iptables 但有 nft——多数发行版的 iptables 是 nft 后端别名，可继续；若容器无法联网需安装 iptables 兼容包"
  else
    bad "系统缺 iptables，docker 端口映射/容器网络将不可用（精简系统常见）。需离线装 iptables 或打包时自带"
  fi

  # systemd
  if [ -d /run/systemd/system ] || pidof systemd >/dev/null 2>&1; then
    ok "systemd 可用，将注册开机自启服务"
  else
    warn "无 systemd，请用: install --no-systemd（前台 nohup 方式拉起 dockerd）"
  fi

  # 已有 docker 残留
  if command -v docker >/dev/null 2>&1; then
    warn "已存在 docker 命令: $(command -v docker)；安装会覆盖二进制，/var/lib/docker 镜像数据保留"
  else ok "无旧版 docker"; fi
  if pidof systemd >/dev/null 2>&1 && systemctl is-active docker >/dev/null 2>&1; then warn "docker 服务运行中，安装时将自动停止"; fi

  # 磁盘
  local avail; avail=$(df -BM --output=avail /var/lib 2>/dev/null | tail -1 | tr -d 'M')
  if [ -n "$avail" ] && [ "$avail" -ge 2048 ]; then ok "/var/lib 可用空间 ${avail}MB"; else warn "/var/lib 可用空间不足 2GB，拉镜像可能失败"; fi

  echo
  if   [ "$RED" -gt 0 ]; then echo "结论: [红]x$RED [黄]x$YEL —— 存在阻断项，请先按红项处理"
  elif [ "$YEL" -gt 0 ]; then echo "结论: [黄]x$YEL —— 可以安装，注意黄项提示"
  else echo "结论: 环境完全兼容，可直接安装 ✓"; fi
  [ "$RED" -eq 0 ] || exit 2
}

# ============ pack：联网机下载并打离线包 ============
pack_one_arch() { # $1=arch $2=payload dir
  local arch="$1" out="$2"
  mkdir -p "$out/bin" "$WORKDIR/docker-$arch"
  log "  下载 Docker ${DOCKER_VERSION} (${arch}) ..."
  curl -fL --retry 3 -o "$WORKDIR/docker-$arch.tgz" "$BASE/$arch/docker-${DOCKER_VERSION}.tgz"
  tar -xzf "$WORKDIR/docker-$arch.tgz" -C "$WORKDIR/docker-$arch" --strip-components=1
  cp "$WORKDIR/docker-$arch/"* "$out/bin/"
  log "  下载 Compose ${COMPOSE_VERSION} (${arch}) ..."
  curl -fL --retry 3 -o "$out/bin/docker-compose" "${COMPOSE_URL}${arch}"
  rm -rf "$WORKDIR/docker-$arch.tgz" "$WORKDIR/docker-$arch"
}

cmd_pack() {
  local multi=0
  [ "${1:-}" = "--multi" ] && multi=1
  local payload="$WORKDIR/payload"
  mkdir -p "$payload"

  if [ "$multi" = 1 ]; then
    log "双架构打包 (x86_64 + aarch64) ..."
    pack_one_arch x86_64 "$payload/x86_64"
    pack_one_arch aarch64 "$payload/aarch64"
  else
    log "单架构打包 ($ARCH) ..."
    pack_one_arch "$ARCH" "$payload/$ARCH"
  fi

  # 安装器（嵌入包内，目标机 root 执行；自动检测架构）
  cat > "$payload/install.sh" <<'INSTALLER'
#!/usr/bin/env bash
# 离线安装器：自动选架构、可选 --no-systemd
set -euo pipefail
cd "$(dirname "$0")"
NO_SYSTEMD=0
for a in "$@"; do [ "$a" = "--no-systemd" ] && NO_SYSTEMD=1; done
log() { echo "[install] $*"; }
[ "$(id -u)" -eq 0 ] || { echo "请用 root / sudo 运行"; exit 1; }

# 架构自适应
case "$(uname -m)" in
  x86_64) ARCH_DIR="x86_64" ;;
  aarch64|arm64) ARCH_DIR="aarch64" ;;
  *) echo "不支持的架构: $(uname -m)"; exit 1 ;;
esac
[ -d "$ARCH_DIR" ] || { echo "包内无 $ARCH_DIR 架构目录"; exit 1; }
log "目标架构: $ARCH_DIR"

# 1) 停旧服务
if pidof systemd >/dev/null 2>&1 && systemctl is-active docker >/dev/null 2>&1; then
  log "停止已有 docker/containerd ..."
  systemctl stop docker docker.socket containerd 2>/dev/null || true
fi
pkill -x dockerd 2>/dev/null && sleep 2 || true

# 2) 二进制
log "安装二进制到 /usr/local/bin ..."
for b in dockerd docker containerd containerd-shim-runc-v2 runc ctr docker-init docker-proxy; do
  [ -f "$ARCH_DIR/bin/$b" ] && install -m 0755 "$ARCH_DIR/bin/$b" /usr/local/bin/
done

# 3) compose 插件（多路径软链，覆盖新旧 CLI 查找路径）
log "安装 compose 插件 ..."
mkdir -p /usr/local/lib/docker/cli-plugins /usr/libexec/docker/cli-plugins /usr/lib/docker/cli-plugins
install -m 0755 "$ARCH_DIR/bin/docker-compose" /usr/local/lib/docker/cli-plugins/docker-compose
for d in /usr/libexec/docker/cli-plugins /usr/lib/docker/cli-plugins; do
  ln -sf /usr/local/lib/docker/cli-plugins/docker-compose "$d/docker-compose"
done

if [ "$NO_SYSTEMD" = 1 ]; then
  # ---- 无 systemd 兜底 ----
  log "以 nohup 方式启动 dockerd（无 systemd 环境）..."
  mkdir -p /etc/docker /var/log
  nohup /usr/local/bin/dockerd >> /var/log/dockerd.log 2>&1 &
  sleep 3
  /usr/local/bin/docker version --format 'Docker {{.Server.Version}} ✓'
  log "开机自启: 请将下行加入 /etc/rc.local:"
  log "  nohup /usr/local/bin/dockerd >> /var/log/dockerd.log 2>&1 &"
else
  # ---- systemd 单元 ----
  log "写入 systemd 单元 ..."
  cat > /etc/systemd/system/containerd.service <<'UNIT'
[Unit]
Description=containerd container runtime
After=network.target local-fs.target
[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
[Install]
WantedBy=multi-user.target
UNIT
  cat > /etc/systemd/system/docker.service <<'UNIT'
[Unit]
Description=Docker Application Container Engine
Documentation=https://docs.docker.com
After=network-online.target containerd.service
Wants=network-online.target
Requires=containerd.service
[Service]
ExecStart=/usr/local/bin/dockerd
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
Delegate=yes
KillMode=process
OOMScoreAdjust=-500
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now containerd
  systemctl enable --now docker
fi

# 4) daemon.json 镜像加速（可选）
if [ -n "${REGISTRY_MIRROR:-}" ]; then
  log "配置镜像加速: $REGISTRY_MIRROR"
  mkdir -p /etc/docker
  python3 - "$REGISTRY_MIRROR" <<'PY' > /etc/docker/daemon.json 2>/dev/null || true
import json,sys
mirrors=[m.strip() for m in sys.argv[1].split(",") if m.strip()]
print(json.dumps({"registry-mirrors":mirrors,"log-driver":"json-file","log-opts":{"max-size":"50m","max-file":"3"}},indent=2))
PY
fi

# 5) 验证
log "验证 ..."
docker version --format 'Docker {{.Server.Version}} ✓'
docker compose version
log "完成！非 root 用户请执行: usermod -aG docker $USER 并重新登录"
INSTALLER
  chmod +x "$payload/install.sh"

  cat > "$payload/README.txt" <<EOF
Docker ${DOCKER_VERSION} + Compose ${COMPOSE_VERSION} 离线安装包
建议流程: 先跑  ./docker-offline-kit.sh doctor  预检，再安装。
安装:     tar xzf *.tar.gz && sudo ./install.sh        # 无 systemd 加 --no-systemd
EOF

  local tag="$DOCKER_VERSION+compose${COMPOSE_VERSION#v}"
  local suffix; [ "$multi" = 1 ] && suffix="multiarch" || suffix="$ARCH"
  local tarball="${KIT_NAME}-${tag}-${suffix}.tar.gz"
  log "打包 -> $tarball"
  tar -czf "$tarball" -C "$payload" .
  rm -rf "$WORKDIR"
  log "完成。目标机流程:"
  log "  ./docker-offline-kit.sh doctor        # 预检"
  log "  tar xzf $(basename "$tarball") && sudo ./install.sh [--no-systemd]"
}

# ============ install：目标机安装 ============
cmd_install() {
  local pkg="" extra=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --no-systemd) extra+=("$1"); shift ;;
      *) pkg="$1"; shift ;;
    esac
  done
  [ -n "$pkg" ] || pkg=$(ls -1 ${KIT_NAME}-*.tar.gz 2>/dev/null | head -1 || true)
  [ -n "$pkg" ] && [ -f "$pkg" ] || die "未找到离线包。用法: $0 install <docker-offline-kit-*.tar.gz> [--no-systemd]"

  # 安装前自动预检
  log "先做安装前预检 ..."
  cmd_doctor || true

  local tmp; tmp=$(mktemp -d /tmp/docker-offline.XXXXXX)
  tar -xzf "$pkg" -C "$tmp"
  REGISTRY_MIRROR="${REGISTRY_MIRROR:-}" bash "$tmp/install.sh" "${extra[@]+"${extra[@]}"}"
  rm -rf "$tmp"
}

# ============ verify ============
cmd_verify() {
  docker version
  docker compose version
  docker info --format 'Storage: {{.Driver}} | Cgroup: {{.CgroupDriver}} | Kernel: {{.KernelVersion}}'
  docker run --rm hello-world >/dev/null 2>&1 && log "hello-world 运行成功 ✓" || log "hello-world 失败（离线环境拉不到镜像属正常，不影响部署本地镜像）"
}

case "${1:-help}" in
  pack)    shift; cmd_pack "$@" ;;
  doctor)  cmd_doctor ;;
  install) shift; cmd_install "$@" ;;
  verify)  cmd_verify ;;
  *) sed -n '2,17p' "$0" ;;
esac
