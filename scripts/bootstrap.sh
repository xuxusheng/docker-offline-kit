#!/usr/bin/env bash
# docker-offline-kit universal 引导壳
# 单文件 = 本脚本 + 追加的多架构 Go 安装器 tar.gz（标记行之后）
# 内嵌 payload 的 sha256（打包时写入 EXPECTED_SHA256），解包前自动校验传输完整性
# 用法: sudo bash docker-offline-installer-*.run [安装器参数]
#       参数原样透传，如 --yes --no-systemd --mirror=... --sudo-pass ...
set -euo pipefail

EXPECTED_SHA256="__DOK_SHA256__"
MARKER="__DOK_PAYLOAD_BELOW__"

# 1) 定位 payload 字节偏移（脚本自身）
OFFSET=$(grep -abm1 "^${MARKER}$" "$0" | cut -d: -f1)
# 标记起始(0基) + 标记长度 + 换行 +1 → tail -c 的 1 基起始位
OFFSET=$((OFFSET + ${#MARKER} + 2))
[ "$OFFSET" -gt 10 ] || { echo "错误: payload 标记未找到（文件损坏？）"; exit 1; }

WORK=$(mktemp -d /tmp/dok-installer.XXXXXX)
trap 'rm -rf "$WORK"' EXIT

# 2) 切出 payload 并校验完整性（防 U 盘/网络传输损坏）
tail -c +"$OFFSET" "$0" > "$WORK/payload.tgz"
if [ ! "$EXPECTED_SHA256" = "__DOK_SHA256__" ]; then
  ACTUAL=$(sha256sum "$WORK/payload.tgz" | awk '{print $1}')
  if [ "$ACTUAL" != "$EXPECTED_SHA256" ]; then
    echo "错误: payload sha256 校验失败（传输损坏？请重新下载）"
    echo "  期望: $EXPECTED_SHA256"
    echo "  实际: $ACTUAL"
    exit 1
  fi
fi
tar -xzf "$WORK/payload.tgz" -C "$WORK" || {
  echo "错误: payload 解压失败"; exit 1;
}

# 3) 架构自适应并执行
case "$(uname -m)" in
  x86_64|amd64)   ARCH="x86_64" ;;
  aarch64|arm64)  ARCH="aarch64" ;;
  armv7l|armv6l)  ARCH="armhf" ;;
  *) echo "不支持的架构: $(uname -m)（支持 x86_64/aarch64/armv7）"; exit 1 ;;
esac

INSTALLER="$WORK/$ARCH"
[ -f "$INSTALLER" ] || { echo "错误: 包内无 $ARCH 架构安装器"; exit 1; }
chmod +x "$INSTALLER"

exec "$INSTALLER" "$@"
