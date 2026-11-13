# Fish 服务端部署指南（2C2G 云服务器）

## 1. 产物

```bash
deploy/build.sh        # Linux/macOS 执行
# 或 Windows 上: powershell deploy/build.ps1
# 产物输出 dist/fish-server-linux-amd64（约 15MB 单二进制）
```

## 2. 首次部署

```bash
# 1) 上传二进制
scp dist/fish-server-linux-amd64 user@server:/tmp/

# 2) 服务器上安装（以普通用户 fish 运行）
sudo useradd -r -s /usr/sbin/nologin fish || true
sudo mkdir -p /opt/fish && sudo mv /tmp/fish-server-linux-amd64 /opt/fish/fish-server
sudo chmod +x /opt/fish/fish-server && sudo chown -R fish:fish /opt/fish

# 3) 首次启动（自动生成 config.json，随机双 token）
sudo -u fish /opt/fish/fish-server -config /opt/fish/config.json
# 记下日志输出的两条 token（分别发给客户端 A、B）；Ctrl+C 退出

# 4) 注册 systemd 服务
sudo cp fish.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now fish
```

## 3. 验证

```bash
curl http://127.0.0.1:8080/healthz          # {"status":"ok"}
# 浏览器打开 http://服务器IP:8080/ 即内置网页客户端
```

## 4. TLS（强烈建议）

服务端本身只提供 HTTP/WS，公网请前置 Caddy 自动 HTTPS：

```
# /etc/caddy/Caddyfile
im.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

客户端填 `https://im.example.com`（SDK 自动转 wss）。若必须直连 IP 裸奔，至少确保 token 足够长（默认生成的 48 位 hex）。

## 5. 数据与备份

| 内容 | 位置 |
|---|---|
| 消息数据库 | `/opt/fish/data/fish.db`（SQLite WAL） |
| 图片文件 | `/opt/fish/data/files/` |
| 配置 | `/opt/fish/config.json`（含密钥，权限 0600） |

```bash
# 冷备（停服后复制，或在线 sqlite3 ".backup"）
sudo systemctl stop fish
tar czf fish-backup-$(date +%F).tgz /opt/fish/data /opt/fish/config.json
sudo systemctl start fish
```

## 6. 历史清理（可选）

消息永久保留。需要清理时：

```bash
sqlite3 /opt/fish/data/fish.db "DELETE FROM messages WHERE ts < (strftime('%ms','now','-90 days')*1);"
```
图片清理需同步删除 `data/files/` 内文件与 `files` 表记录（建议直接整目录冷备后清空并重置库）。

## 7. 容量参考（2C2G）

| 项 | 建议上限 |
|---|---|
| 内存 | 服务端常态 < 50MB，systemd 限 256M |
| 并发连接 | 同身份 3~5 个设备足够 |
| 消息 QPS | 1:1 场景数百条/秒无压力 |
| 图片 | 单文件 ≤ 10MB（config.json 可调） |

## 8. 防火墙

仅需放行 8080（或 Caddy 的 80/443）。WS 与 HTTP 同端口。
