#!/usr/bin/env bash
# gotorrent systemd 部署脚本
#
# 用法:
#   sudo ./deploy/install.sh
#
# 前置:
#   - 已编译好的 gotorrent 二进制(或先运行 go build -o gotorrent .)
#   - systemd 环境

set -euo pipefail

BIN_SRC="${1:-./gotorrent}"
BIN_DST="/usr/local/bin/gotorrent"
USER_NAME="gotorrent"
HOME_DIR="/var/lib/gotorrent"
DL_DIR="${HOME_DIR}/downloads"
ENV_FILE="/etc/gotorrent/gotorrent.env"
UNIT_DST="/etc/systemd/system/gotorrent.service"

if [[ $EUID -ne 0 ]]; then
    echo "请用 sudo 运行此脚本" >&2
    exit 1
fi

if [[ ! -x "$BIN_SRC" ]]; then
    echo "未找到可执行文件 $BIN_SRC;请先编译: go build -o gotorrent ." >&2
    exit 1
fi

echo "[1/6] 创建用户 ${USER_NAME}..."
id "$USER_NAME" &>/dev/null || useradd --system --home-dir "$HOME_DIR" --create-home --shell /usr/sbin/nologin "$USER_NAME"

echo "[2/6] 创建目录..."
mkdir -p "$DL_DIR"
chown -R "$USER_NAME":"$USER_NAME" "$HOME_DIR"

echo "[3/6] 安装二进制到 ${BIN_DST}..."
install -m 0755 "$BIN_SRC" "$BIN_DST"

echo "[4/6] 写入环境文件 ${ENV_FILE}..."
mkdir -p /etc/gotorrent
if [[ ! -f "$ENV_FILE" ]]; then
    # 生成随机 RPC 令牌
    RPC_SECRET=$(head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32)
    cat > "$ENV_FILE" <<EOF
RPC_SECRET=${RPC_SECRET}
EOF
    chmod 600 "$ENV_FILE"
    chown root:root "$ENV_FILE"
    echo "  已生成 RPC_SECRET 并写入 ${ENV_FILE}(chmod 600)"
else
    echo "  ${ENV_FILE} 已存在,跳过"
fi

echo "[5/6] 安装 systemd unit..."
install -m 0644 "$(dirname "$0")/gotorrent.service" "$UNIT_DST"

echo "[6/6] 启用并启动服务..."
systemctl daemon-reload
systemctl enable --now gotorrent.service

echo ""
echo "部署完成。"
echo "  服务状态: systemctl status gotorrent"
echo "  查看日志: journalctl -u gotorrent -f"
echo "  控制面板: http://127.0.0.1:6800/ui"
echo "  RPC 令牌: cat ${ENV_FILE}"
