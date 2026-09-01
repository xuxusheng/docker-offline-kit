#!/usr/bin/env bash
# QEMU 全系统模拟实测 armv7 安装器
# 在容器内跑 qemu-system-arm + Debian armhf 云镜像，真内核+真 systemd，
# 完整验证 installer-arm（doctor→sudo→systemd 注册→启动→自验）。
# 用法: scripts/test/qemu-armhf.sh [installer路径]  （默认 dist/installer-arm）
set -euo pipefail
cd "$(dirname "$0")/../.."

INSTALLER="${1:-dist/installer-arm}"
WORK=".qemu-armhf"
PUBKEY=$(cat ~/.ssh/id_ed25519.pub)

mkdir -p "$WORK"

# ---------- 1) 构建带 QEMU 的容器镜像 ----------
docker build -q -t dok-qemu -f- . <<'DOCKERFILE' >/dev/null
FROM debian:12
RUN apt-get update -qq && apt-get install -y -qq \
    qemu-system-arm qemu-utils cloud-image-utils sshpass openssh-client u-boot-qemu \
    >/dev/null && rm -rf /var/lib/apt/lists/*
DOCKERFILE

# ---------- 2) 下载 Debian 12 armhf 云镜像（幂等）----------
IMG="$WORK/debian-12-generic-armhf.qcow2"
if [ ! -f "$IMG" ]; then
  echo "==> 下载 Debian armhf 云镜像（~350MB）..."
  docker run --rm -v "$PWD/$WORK:/w" debian:12 bash -c '
    curl -fL --retry 3 -o /w/img.qcow2 "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-generic-armhf.qcow2"
    ' 
  mv "$WORK/img.qcow2" "$IMG"
fi

# ---------- 3) cloud-init seed（注入 dsh 公钥，debian 用户 sudo 免密）----------
docker run --rm -v "$PWD/$WORK:/w" -e PUBKEY="$PUBKEY" dok-qemu bash -c '
cat > /w/user-data <<EOF
#cloud-config
users:
  - name: debian
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $PUBKEY
ssh_pwauth: false
EOF
echo "instance-id: dok-armhf-001" > /w/meta-data
cloud-localds /w/seed.iso /w/user-data /w/meta-data
echo "seed.iso 生成完成"'

# ---------- 4) 启动 QEMU（TCG 模拟，hostfwd 2222->22）----------
pkill -f "qemu-system-arm.*dok-armhf" 2>/dev/null || true
sleep 1
echo "==> 启动 QEMU (armv7 TCG)..."
docker run -d --name dok-qemu-run --rm \
  -v "$PWD/$WORK:/w" -v "$PWD/$INSTALLER:/installer:ro" \
  -p 127.0.0.1:2222:22 \
  dok-qemu bash -c '
  qemu-system-arm \
    -M virt -cpu cortex-a15 -smp 2 -m 1024 \
    -kernel /usr/lib/u-boot/qemu_arm/u-boot.bin \
    -drive if=virtio,format=qcow2,file=/w/debian-12-generic-armhf.qcow2 \
    -drive if=virtio,format=raw,file=/w/seed.iso \
    -netdev user,id=n0,hostfwd=tcp::22-:22 \
    -device virtio-net-pci,netdev=n0 \
    -display none -serial file:/w/serial.log \
    -pidfile /w/qemu.pid' 
sleep 3
docker ps --filter name=dok-qemu-run --format "{{.Status}}" | head -1

# ---------- 5) 等 SSH 就绪（TCG 启动较慢，最多 12 分钟）----------
echo "==> 等待 VM SSH 就绪（TCG 模拟较慢）..."
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -p 2222"
UP=0
for i in $(seq 1 72); do
  if sshpass -p nopass ssh $SSH_OPTS debian@127.0.0.1 true 2>/dev/null; then UP=1; break; fi
  sleep 10
done
[ "$UP" = 1 ] || { echo "✗ VM 未能启动，串口日志尾部："; tail -30 "$WORK/serial.log" 2>/dev/null; docker logs dok-qemu-run 2>&1 | tail -10; exit 1; }
echo "==> VM 就绪（用时约 $((i*10))s）"

# ---------- 6) 跑安装器（sudo 免密路径 + systemd 真环境）----------
echo "==> VM 信息："
sshpass -p nopass ssh $SSH_OPTS debian@127.0.0.1 'uname -m && grep PRETTY /etc/os-release && echo "docker: $(command -v docker || echo 无)"'
echo "==> 运行安装器（--non-interactive --yes，sudo 免密）"
sshpass -p nopass ssh $SSH_OPTS debian@127.0.0.1 \
  'sudo /installer --non-interactive --yes 2>&1 | grep -Ev "总进度" | tail -22'
echo "==> 安装后验证："
sshpass -p nopass ssh $SSH_OPTS debian@127.0.0.1 '
  systemctl is-active docker containerd | tr "\n" " "; echo
  docker version --format "Docker {{.Server.Version}} ✓"
  docker compose version
  docker info --format "Storage: {{.Driver}} | Cgroup: {{.CgroupVersion}}"
  sudo usermod -aG docker debian
  echo "(拉镜像外网受限则容器运行测试以本机验证为准)"'

# ---------- 7) 清理 ----------
docker stop dok-qemu-run >/dev/null 2>&1 || true
echo "✓ armv7 QEMU 实测完成"
