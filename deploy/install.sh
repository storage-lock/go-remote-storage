#!/usr/bin/env bash
# 在 Linux VPS 上安装远程存储服务。
#
#   sudo ./deploy/install.sh
#
# 做的事：
#   1. 建系统用户与目录
#   2. 生成访问 token（首次）
#   3. 编译安装二进制
#   4. 安装 systemd unit 并启动
#   5. 打印 nginx/TLS 下一步
set -euo pipefail

APP=remote-storage
BIN=/usr/local/bin/remote-storage-server
DATA=/var/lib/remote-storage
CONF=/etc/remote-storage
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ $EUID -ne 0 ]]; then
  echo "请用 root 运行：sudo $0" >&2
  exit 1
fi

echo "==> 创建用户与目录"
id -u "$APP" >/dev/null 2>&1 || useradd --system --shell /usr/sbin/nologin --home "$DATA" "$APP"
install -d -o "$APP" -g "$APP" -m 0750 "$DATA"
install -d -o root -g "$APP" -m 0750 "$CONF"

echo "==> 生成访问 token（仅首次）"
if [[ ! -f "$CONF/server.env" ]]; then
  TOKEN=$(head -c 32 /dev/urandom | base64)
  umask 077
  printf 'REMOTE_STORAGE_TOKEN=%s\n' "$TOKEN" > "$CONF/server.env"
  chown root:"$APP" "$CONF/server.env"
  echo "已写入 $CONF/server.env"
  echo
  echo "  客户端要用的 token："
  echo "    $TOKEN"
  echo
  echo "  ⚠️ 请立刻抄走并妥善保存；文件权限 600，只有 root 和 $APP 组可读。"
else
  echo "$CONF/server.env 已存在，保留原 token"
fi

echo "==> 编译"
if ! command -v go >/dev/null; then
  echo "未找到 Go 工具链；请安装 Go 1.23+ 或改用 deploy/Dockerfile" >&2
  exit 1
fi
cd "$SRC"
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o "$BIN" ./server/cmd/remote-storage-server
chmod 0755 "$BIN"

echo "==> 安装 systemd unit"
cp "$SRC/deploy/systemd/remote-storage-server.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now remote-storage-server

sleep 1
if systemctl is-active --quiet remote-storage-server; then
  echo "==> 服务已启动"
  curl -fsS http://127.0.0.1:8443/v1/health && echo
else
  echo "启动失败：journalctl -u remote-storage-server -n 50" >&2
  exit 1
fi

cat <<'TIP'

下一步（公网接入）：
  1. 把 deploy/nginx.conf 里的 storage.example.com 换成你的域名
  2. certbot --nginx -d storage.example.com
  3. nginx -t && systemctl reload nginx
  4. 客户端：
       BASE=https://storage.example.com
       TOKEN=<上面那个 token>

服务只监听 127.0.0.1:8443。不要直接暴露到公网——token 会明文传输。
TIP
