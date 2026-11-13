#!/usr/bin/env bash
# 服务器一次性初始化脚本
# 功能：创建 fish 用户、准备部署目录、安装 systemd 服务单元
# 用法：
#   ssh <user>@42.121.192.198
#   sudo bash server-init.sh
#
# 注意：
#   1. 运行前需先把当前仓库 git clone 到服务器（或用 CI 上传脚本）
#   2. config.json 需手动放到 /opt/fish/config.json（含 token，不入库）
#   3. 运行完成后，服务不会立即启动，等 CI 首次 push 上传二进制后才会生效

set -euo pipefail

echo "==> 1. 创建 fish 运行用户"
if ! id -u fish &>/dev/null; then
  useradd --system --no-create-home --shell /usr/sbin/nologin fish
  echo "已创建 fish 用户 (UID=$(id -u fish))"
else
  echo "fish 用户已存在"
fi

echo "==> 2. 创建部署目录"
mkdir -p /opt/fish
chown fish:fish /opt/fish
chmod 750 /opt/fish

echo "==> 3. 安装 systemd 服务单元"
if [ -f fish.service ]; then
  cp -a fish.service /etc/systemd/system/fish.service
  echo "已复制 fish.service"
elif [ -f deploy/fish.service ]; then
  cp -a deploy/fish.service /etc/systemd/system/fish.service
  echo "已复制 deploy/fish.service"
else
  echo "⚠ 找不到 fish.service，可从仓库 deploy/fish.service 手动复制到 /etc/systemd/system/"
fi

systemctl daemon-reload

echo "==> 4. 预置目录权限"
chown -R fish:fish /opt/fish

echo ""
echo "==> 完成。后续步骤："
echo "  1) 在 /opt/fish/config.json 写入配置（模板见 config.example.json）"
echo "  2) 在 GitHub 仓库 Settings → Secrets 中配置 4 个变量："
echo "     DEPLOY_HOST=42.121.192.198"
echo "     DEPLOY_USER=<你的服务器登录用户>"
echo "     DEPLOY_SSH_KEY=<ssh 私钥内容>"
echo "     DEPLOY_PORT=22"
echo "  3) 首次 push 到 master/main 分支触发 CI 部署，CI 会自动上传二进制并 systemctl enable fish"
echo "  4) 验证：curl http://127.0.0.1:19090/healthz"
