#!/usr/bin/env bash
# QEMU 全系统模拟实测 armhf 安装器（自建 rootfs 路线）
# 背景：Debian 12 官方云镜像已无 armhf，改用 debootstrap 自建 armhf 根文件系统
# （真 armhf 内核 linux-image-armmp + 真 systemd），完整验证安装器 systemd 路径。
# 用法: scripts/test/qemu-armhf.sh [installer路径]
set -euo pipefail
cd "$(dirname "$0")/../.."

INSTALLER="${1:-dist/installer-armhf}"
WORK=".qemu-armhf"
PUBKEY=$(cat ~/.ssh/id_ed25519.pub)
mkdir -p "$WORK"

docker build -q -t dok-qemu      -f scripts/test/Dockerfile.qemu . >/dev/null
docker build -q -t dok-qemu-build -f scripts/test/Dockerfile.qemu-build . >/dev/null

# ---------- 1) debootstrap armhf rootfs + 制盘（容器内完成，幂等）----------
if [ ! -f "$WORK/disk.qcow2" ]; then
  echo "==> debootstrap armhf rootfs（qemu-user 转译，约 10-25 分钟）..."
  docker run -i --rm --privileged -v "$PWD/$WORK:/w" -v "$PWD/$INSTALLER:/installer:ro" -e PUBKEY="$PUBKEY" \
    dok-qemu-build bash -ex <<'BUILD'
update-binfmts --enable qemu-arm 2>/dev/null || true
# 1a) debootstrap（--include 一次装齐：真内核+systemd+ssh+sudo）
debootstrap --arch=armhf --variant=minbase \
  --include=systemd,systemd-sysv,openssh-server,sudo,iptables,xz-utils,isc-dhcp-client,linux-image-armmp,initramfs-tools \
  bookworm /srv/r http://deb.debian.org/debian
# 1b) chroot 配置（qemu-arm-static binfmt 转译 arm 二进制）
cp /usr/bin/qemu-arm-static /srv/r/usr/bin/
mount --bind /proc /srv/r/proc
mount --bind /sys  /srv/r/sys
mount --bind /dev  /srv/r/dev
chroot /srv/r bash -ex <<'CHROOT'
useradd -m -s /bin/bash -G sudo debian
echo 'debian ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/debian
mkdir -p /home/debian/.ssh
echo "$PUBKEY" > /home/debian/.ssh/authorized_keys
chown -R debian:debian /home/debian/.ssh
chmod 700 /home/debian/.ssh && chmod 600 /home/debian/.ssh/authorized_keys
printf '/dev/vda1  /  ext4  defaults  0 1\n' > /etc/fstab
cat > /etc/systemd/network/10-eth0.network <<'NET'
[Match]
Name=eth0
[Network]
DHCP=yes
NET
systemctl enable systemd-networkd systemd-resolved ssh 2>/dev/null || true
echo debian > /etc/hostname
CHROOT
umount /srv/r/proc /srv/r/sys /srv/r/dev
# 1c) 制 3GB ext4 盘
dd if=/dev/zero of=/w/disk.raw bs=1M count=3072 status=none
parted -s /w/disk.raw mklabel gpt mkpart primary ext4 1MiB 100%
mkfs.ext4 -F -q /w/disk.raw
mkdir -p /mnt/disk
mount -o loop /w/disk.raw /mnt/disk
rsync -a /srv/r/ /mnt/disk/
ls /mnt/disk/boot/ | head -4
umount /mnt/disk
qemu-img convert -O qcow2 /w/disk.raw /w/disk.qcow2
rm /w/disk.raw
echo "==> rootfs 盘制作完成"
BUILD
fi

# ---------- 2) 从盘里拷出内核/initrd ----------
docker run -i --rm --privileged -v "$PWD/$WORK:/w" dok-qemu-build bash -ex <<'KERN'
[ -f /w/vmlinuz ] && [ -f /w/initrd.img ] && { echo "内核已提取"; exit 0; }
qemu-img convert -O raw /w/disk.qcow2 /w/disk.raw
mkdir -p /mnt/disk
mount -o loop,ro /w/disk.raw /mnt/disk
cp /mnt/disk/boot/vmlinuz-* /w/vmlinuz
cp /mnt/disk/boot/initrd.img-* /w/initrd.img
umount /mnt/disk
rm /w/disk.raw
ls -la /w/vmlinuz /w/initrd.img
KERN

# ---------- 3) 启动 QEMU ----------
pkill -f "qemu-system-arm.*dok" 2>/dev/null || true; sleep 1
echo "==> 启动 QEMU (armv7 全系统, TCG)..."
docker run -d --name dok-qemu-run --rm \
  -v "$PWD/$WORK:/w" -v "$PWD/$INSTALLER:/installer:ro" \
  -p 127.0.0.1:2222:22 dok-qemu bash -c '
  qemu-system-arm -M virt -cpu cortex-a15 -smp 2 -m 1024 \
    -kernel /w/vmlinuz -initrd /w/initrd.img \
    -append "root=/dev/vda1 rw console=ttyAMA0" \
    -drive if=virtio,format=qcow2,file=/w/disk.qcow2 \
    -netdev user,id=n0,hostfwd=tcp::22-:22 \
    -device virtio-net-pci,netdev=n0 \
    -display none -serial file:/w/serial.log'
sleep 3; docker ps --filter name=dok-qemu-run --format "{{.Status}}"

# ---------- 4) 等 SSH（TCG 慢，最长 15 分钟）----------
echo "==> 等待 VM SSH 就绪..."
UP=0
for i in $(seq 1 90); do
  sshpass -p nopass ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 -p 2222 debian@127.0.0.1 true 2>/dev/null && UP=1 && break
  sleep 10
done
[ "$UP" = 1 ] || { echo "✗ VM 未启动，串口日志尾部:"; tail -40 "$WORK/serial.log" 2>/dev/null; exit 1; }
echo "==> VM 就绪（约 $((i*10))s）"

# ---------- 5) 跑安装器 ----------
SSHCMD="sshpass -p nopass ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p 2222 debian@127.0.0.1"
echo "==> VM: $($SSHCMD 'uname -m; grep PRETTY /etc/os-release' 2>/dev/null | tr '\n' ' ')"
echo "==> 运行安装器（sudo 免密 + systemd 路径）"
$SSHCMD 'sudo /installer --non-interactive --yes 2>&1 | grep -Ev "总进度" | tail -20'
echo "==> 安装后验证:"
$SSHCMD 'systemctl is-active docker containerd | tr "\n" " "; echo
  docker version --format "Docker {{.Server.Version}} ✓"
  docker compose version
  docker info --format "Storage: {{.Driver}} | Cgroup: {{.CgroupDriver}}"' 2>/dev/null

docker stop dok-qemu-run >/dev/null 2>&1 || true
echo "✓ armhf QEMU 全系统实测完成"
