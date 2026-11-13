# Fish 单机测试方案与验收标准

适用范围：开发机（Windows）单机模拟双端完整链路。服务器部署后的冒烟验证见 `deploy/README.md` §3。

---

## 一、测试环境准备

### 1. 构建并启动服务端

```powershell
# 构建
go build -o dist\fish-server.exe .\cmd\server

# 准备 config.json（endpoints 字段已在动态配对体系下弃用，可留空数组或省略）
# 最简模板：
{
  "listen": ":19090",
  "data_dir": "./data"
}

# 启动
.\dist\fish-server.exe -config config.json
```

**首次启动**会在 `data_dir/jwt.key` 生成 32 字节 HMAC 密钥（持久化，重启不失效）。`data_dir/fish.db` 自动创建，空库。

### 2. 清空旧数据库（切到新鉴权体系后，旧预设 token 时代的 db 无意义）

```powershell
# 若想全新开始（清掉所有历史消息和房间密码），删除 data 目录下 db + jwt.key
Remove-Item -Recurse -Force .\data
```

### 3. 双端模拟

两个浏览器窗口（推荐 Chrome 普通窗口 + 无痕窗口，localStorage 隔离），同时访问 `http://127.0.0.1:19090/`：

- **第一个窗口**：输入密码 → 点「创建新房间」 → 自动成为角色 A
- **第二个窗口**：输入同一密码 → 点「已有房间，加入」 → 自动成为角色 B

同一密码房间允许多设备同时在线。

---

## 二、自动化测试（已内置，回归用）

```powershell
go test -timeout 180s ./...
```

全绿标准：所有包 OK，exit code 0。

### 各包覆盖场景

| 包 | 用例数 | 覆盖 |
|---|---|---|
| `internal/auth` | 11 | `Init`（首台自动 A、重复 Init 拒绝、输入校验）；`Login`（自动分配 B、密码错误、Room 未初始化、已注册设备保持 role、A/B 都存在时显式 desired_role）；`Verify` JWT 验签；`PeerOf` 包级函数 |
| `internal/store` | 4 | 消息表 seq 单调、Get/List/分页/has_more、重启持久化、room/devices 表 CREATE TABLE IF NOT EXISTS 幂等 |
| `internal/api` (ws) | 5 | Init 成功+重复拒绝、Login 拿 B、BadPassword 拒绝、首帧非 auth 拒绝、Ping-Pong、Healthz |
| `internal/api` (files) | 5 | 图片上传嗅探、401/400/404 边界、E2E 图片消息完整链路 |
| `internal/msg` | 4 | 在线互发（A→B→ack 链路）、离线补投顺序与 seq 一致、**多设备同一 role 同时收消息**（A1+A2 同时在线时 B 发一条两边都收到且 seq 相同）、错误路径（未知帧 / 空 body / ack 不存在 seq） |
| `client` | 4 | SDK Init+Login 双端点同时就绪；SendText → 对端收到；Resume 恢复连接后 role 保持；WrongPassword 拒绝 |

---

## 三、手动测试场景（按序执行）

### S1 房间初始化与配对

| 步骤 | 操作 | 预期 |
|---|---|---|
| 1 | 清空旧数据（删 `.\data` 目录），启动服务 | 日志显示「存储就绪: 当前最大 seq=0」「已生成并保存 JWT key」 |
| 2 | 窗口 A 打开 UI，输入密码（如 `secret1234`），点**创建新房间** | 状态栏显示「身份 A · 对端 B 离线」；聊天区空 |
| 3 | 窗口 B（无痕）打开同地址，输入同一密码，点**已有房间，加入** | 状态栏显示「身份 B · 对端 A 在线」 |
| 4 | 观察窗口 A | 状态栏**无需刷新**即变为「身份 A · 对端 B 在线」（服务端 presence 推送） |

### S2 在线互发

| 步骤 | 操作 | 预期 |
|---|---|---|
| 5 | A 发「hello-b」 | A 气泡立即出现在右侧，seq 从 1 开始递增 |
| 6 | 观察 B | 「hello-b」出现在 B 左侧，秒级到达，显示「角色 A」 |
| 7 | B 发「hello-a」 | A 收到，显示「角色 B」 |

### S3 图片消息

| 步骤 | 操作 | 预期 |
|---|---|---|
| 8 | A 点 🖼 选一张 <10MB 真图片发送 | A 侧图片气泡渲染，显示尺寸与大小 |
| 9 | 观察 B | 图片自动加载显示，点击可看原图 |
| 10 | A 尝试发 >10MB 文件或 `.txt` 改名 `.png` | 上传被拒绝（alert 提示） |

### S4 离线补投 + Presence + 刷新恢复

| 步骤 | 操作 | 预期 |
|---|---|---|
| 11 | 关闭窗口 B（模拟离线） | A 状态栏**无需刷新**即变为「对端 B 离线」 |
| 12 | A 连发「offline-1」「offline-2」 | 两条正常发送，气泡回填 seq |
| 13 | 重开 B 窗口（无痕）→ 密码 → 加入 | B 进入后自动 `sync after:0`，历史全部显示（含 offline-1/2 与图片），**每条仅一次**（按 seq 去重） |
| 14 | A 关掉 B 标签后再开 B 标签，加入 | A 状态栏再次实时变「对端 B 在线」 |
| 15 | **刷新窗口 A**（F5，消息区清空，A 掉线）→ B 发「refresh-test」→ A 点恢复连接 | A 消息区显示**全部历史 + refresh-test**，每条仅一次。localStorage 持久化 device_id/token，A 自动走 `resume` 流程，无需重新输入密码 |

### S5 多设备同角色

| 步骤 | 操作 | 预期 |
|---|---|---|
| 16 | 开**第二个无痕窗口** C，点「已有房间，加入」，身份下拉选**角色 A**，输入密码 | C 进入，状态栏「身份 A · 对端 B 在线」。此时角色 A 有两台设备（窗口 A + 窗口 C） |
| 17 | B 发「to-all-As」 | 窗口 A **和** 窗口 C **都**收到「to-all-As」，seq 相同，显示「角色 B」 |
| 18 | 关闭窗口 C | A 状态栏不变（B 仍在线，presence 只在 role 从 1→0 时广播） |

### S6 安全与边界

| 步骤 | 操作 | 预期 |
|---|---|---|
| 19 | 窗口 C 输入**错误密码**，点加入 | 被拒绝，错误提示「密码错误」 |
| 20 | 输入空密码点创建新房间 | 被拒绝 |
| 21 | A 发超长文本（>8KB） | 服务端拒绝（error 4003），UI 不崩溃，气泡撤回 |
| 22 | 直接访问 `http://127.0.0.1:19090/api/files/xxxx?token=任意` | 401 invalid token（JWT 签名不通过） |

### S7 服务重启持久化

| 步骤 | 操作 | 预期 |
|---|---|---|
| 23 | 停止服务进程 → 重新启动（**不要删除 data 目录**） | 启动日志「当前最大 seq=N」（N>0），JWT key 从 `data/jwt.key` 读取不重新生成 |
| 24 | 刷新 A、B 两窗口 | 全部历史消息完整恢复，无需重新输入密码（device_id + token 已 localStorage 持久化，自动 resume） |

---

## 四、验收标准（全部满足即验收通过）

### 功能验收（服务端）
- [ ] AC-1 空库可启动，无预设 token 或账号
- [ ] AC-2 首台设备 `Init(password)` 成为 role A
- [ ] AC-3 同一密码下，新设备自动分配 role B（A/B 都存在时需显式 desired_role）
- [ ] AC-4 错误密码登录被拒（ErrBadPassword）；未初始化房间 Login 被拒（ErrRoomNotInit）
- [ ] AC-5 A、B 互发文本实时到达（<1s 本机）
- [ ] AC-6 接收方离线期间消息补投成功，顺序正确、无重复
- [ ] AC-7 role A 多设备同时在线时，发给 A 的消息推送到**所有**在线设备
- [ ] AC-8 对端上线/下线后，本端状态栏 2s 内实时更新（presence 推送）
- [ ] AC-9 图片上传/下载/类型嗅探/目录穿越全部正确
- [ ] AC-10 服务重启后全部历史消息 + 房间密码 + JWT key 均持久化

### 客户端验收
- [ ] AC-11 Web UI：密码创建房间 / 加入房间 → 正确进入聊天界面
- [ ] AC-12 Web UI：localStorage 持久化 device_id/token/role，下次打开自动 resume（无重新输入密码）
- [ ] AC-13 Web UI：刷新后历史消息**完整显示**（localSeq 不跨页面持久化，重连走全量 sync after:0）
- [ ] AC-14 SDK：`client.Connect({Action: "init"|"login"|"resume"})` 三种动作均可正确握手；JWT 验签通过；事件回调正常

### 协议验收
- [ ] AC-15 自动化测试全绿：`go test ./...` 全部通过
- [ ] AC-16 WS 首帧必须为 `auth`（非 auth → 拒绝），`auth.result` 帧成功/失败语义清晰
- [ ] AC-17 客户端按 seq 幂等去重（重放 sync 不产生重复气泡）
- [ ] AC-18 `msg.send` → `msg.ack` 按 `id/ref` 关联，无串扰

### 部署验收（2C2G 目标）
- [ ] AC-19 无任何重型中间件：仅 SQLite 单文件 + 磁盘文件 + 单 Go 二进制
- [ ] AC-20 服务端内存常态 < 100MB（本机 `Get-Process fish-server` 观察 WorkingSet64）
- [ ] AC-21 部署脚本：`curl http://host:port/healthz` 返回 `{"status":"ok"}`

---

## 五、常见问题

| 现象 | 处理 |
|---|---|
| 密码忘了，想重开房间 | 删掉服务端 `data_dir` 下的 `fish.db` + `jwt.key`，重启服务 → 房间重置为空 |
| 浏览器 localStorage 串号（不同身份显示同一窗口） | 普通窗口 + 无痕窗口分开，或不同浏览器。localStorage 按域名共享 |
| 服务重启后 JWT key 重新生成 → 旧 token 失效 | 删掉了 `data/jwt.key` 才会重新生成；如果想保持 token 有效**不要删这个文件** |
| 图片上传 400 | 真图片才过（服务端嗅探魔数，改扩展名无效）；或 >10MB |
| 端口被占用 | Windows 查 `netstat -ano \| findstr :19090` 找 PID → `Stop-Process -Id PID -Force` |
| 服务端启动报 `endpoints 必须恰好配置 2 个` | config.json 还是旧模板。删掉 `endpoints` 字段或改为空数组 `[]` 即可。动态配对体系已不再需要预设 token |
