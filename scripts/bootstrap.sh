#!/usr/bin/env bash
# docker-offline-kit universal 引导壳
# 单文件 = 本脚本 + 追加的多架构 Go 安装器 tar.gz（标记行之后）
# 用法: sudo bash docker-offline-installer-*.run [安装器参数]
#       参数原样透传，如 --yes --no-systemd --mirror=... --sudo-pass ...
set -euo pipefail

MARKER="__DOK_PAYLOAD_BELOW__"

# 1) 定位 payload 字节偏移（脚本自身）
OFFSET=$(grep -abm1 "^${MARKER}$" "$0" | cut -d: -f1)
# 标记起始(0基) + 标记长度 + 换行 +1 → tail -c 的 1 基起始位
OFFSET=$((OFFSET + ${#MARKER} + 2))
[ "$OFFSET" -gt 10 ] || { echo "错误: payload 标记未找到（文件损坏？）"; exit 1; }

# 2) 解出各架构安装器
WORK=$(mktemp -d /tmp/dok-installer.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
tail -c +"$OFFSET" "$0" | tar -xzf - -C "$WORK" || {
  echo "错误: payload 解压失败（传输损坏？请校验 sha256）"; exit 1;
}

# 3) 架构自适应并执行
case "$(uname -m)" in
  x86_64|amd64)   ARCH="x86_64" ;;
  aarch64|arm64)  ARCH="aarch64" ;;
  armv7l|armv6l)  ARCH="arm" ;;
  *) echo "不支持的架构: $(uname -m)（支持 x86_64/aarch64/armv7）"; exit 1 ;;
esac

INSTALLER="$WORK/$ARCH"
[ -f "$INSTALLER" ] || { echo "错误: 包内无 $ARCH 架构安装器"; exit 1; }
chmod +x "$INSTALLER"

exec "$INSTALLER" "$@"
