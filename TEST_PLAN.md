# Fish 测试方案与验收标准

适用版本：当前 `main`（零配置启动 / 帧内 auth 握手 / 同角色多设备 / token 吊销 + 登录限流）。
适用范围：开发机（Windows）单机模拟双端与多端完整链路；服务器部署后的验证见第四节。

---

## 一、测试环境准备

### 1. 依赖

- Go：以 [go.mod](file:///c:/Documents/Projects/Fish/go.mod) 声明为准（当前 1.27.4）
- 无需 gcc：SQLite 使用纯 Go 驱动（`modernc.org/sqlite`），`CGO_ENABLED=0` 即可
- 浏览器：Chrome/Edge 普通窗口 + 无痕窗口（`localStorage` 隔离，避免身份串号）
- 网络：首次构建需拉依赖，国内可先设置 `$env:GOPROXY = "https://goproxy.cn,direct"`

### 2. 构建与启动

```powershell
# 构建
go build -o dist\fish-server.exe .\cmd\server

# 启动方式一：纯零配置（默认 listen :8080，数据目录 .\data）
.\dist\fish-server.exe

# 启动方式二：命令行覆盖（推荐测试用）
.\dist\fish-server.exe -listen :19090 -data-dir .\data

# 启动方式三：环境变量（优先级低于命令行）
$env:FISH_LISTEN = ':19090'
$env:FISH_DATA_DIR = '.\data'
.\dist\fish-server.exe

# 启动方式四：可选配置文件（只写需要覆盖的字段；文件不存在不报错）
'{ "max_file_bytes": 20971520 }' | Set-Content -Encoding utf8 config.json
.\dist\fish-server.exe -config config.json -listen :19090
```

优先级：**默认值 < config.json < 环境变量 FISH_* < 命令行参数**。

**首次启动预期日志（顺序固定）：**

```
配置就绪: listen=... data_dir=... max_text=8192B max_file=10485760B（配置文件: config.json）
存储就绪: 当前最大 seq=0
已生成并保存 JWT key 到 ...\data\jwt.key
鉴权就绪（动态配对模式，JWT key 持久化）
服务启动: http://127.0.0.1:19090  (健康检查 /healthz, WS /ws)
```

数据目录内容：`fish.db`（+ `-wal`/`-shm`）、`jwt.key`、`files/`（上传后生成）。**不应生成 config.json**。

### 3. 重置环境

```powershell
# 完全重置：清房间密码、历史消息、JWT key、上传文件
# 注意：服务需先停止，否则 Windows 会因文件占用失败
Remove-Item -Recurse -Force .\data
```

### 4. 端口占用排查

```powershell
netstat -ano | findstr :19090
Stop-Process -Id <PID> -Force
```

> 提示：如果本机 19090 上是旧版本进程，测试前务必确认 `/healthz` 正常且日志版本是当前代码，否则会出现"修复未生效"的假象。

---

## 二、自动化测试（回归基线）

```powershell
go build ./...
go vet ./...
go test -timeout 180s ./...
```

全部通过标准：所有包 `ok`、exit code 0。共 **55** 个用例。

| 包 | 用例数 | 覆盖点 |
|---|---|---|
| `internal/config` | 8 | 无配置文件/空路径、部分字段合并、env 覆盖、非法 env 报错、BOM、零值归一、`FISH_CONFIG` 路径 |
| `internal/auth` | 13 | init 首台 A / 重复拒绝 / 输入校验 / **并发只成功一次**；login 自动 B / 密码错 / 未初始化 / 已注册保持 role / 双角色显式指定；JWT 验签；**resume 一致性校验**；PeerOf |
| `internal/store` | 4 | seq 单调、按 seq 读取、分页与 has_more、重启持久化 |
| `internal/api` (files) | 6 | 上传下载回环、类型嗅探拒绝、**超限整体拒绝（不截断）**、401/400/404、图片消息 E2E |
| `internal/api` (ws) | 10 | init/login 角色、同角色回显、**resume 校验与 token 换发**、**帧版本 4003**、**登录限流**、坏密码、首帧非 auth、健康检查、ping-pong |
| `internal/msg` | 4 | 在线互发 + ack、离线补投顺序、多设备同角色广播、错误帧 |
| `client` | 10 | init/login 就绪、发送接收、resume、坏密码、sync、多设备并发发送、图片上传下载、delivered 回执、**断线重连**、**回调内调用同步方法** |

常用单测命令：

```powershell
go test ./client/ -run 'TestCallbackCanSend|TestReconnectAfterClose' -count=1 -v
go test ./internal/api/ -run 'TestWS_' -count=1 -v
go test ./internal/auth/ -run 'TestInit_Concurrent|TestResume_' -count=1 -v
```

> 本机无 gcc，`go test -race` 不可用；如需竞态检测请在 Linux/macOS 或 CI 中执行。

---

## 三、手动测试场景（按序执行）

> 每个场景都假设上一步已完成；任一步不符合预期即判 FAIL，记录现象与服务端日志。

### S0 服务器地址与自动恢复

| 步骤 | 操作 | 预期 |
|---|---|---|
| 1 | 浏览器打开 `http://127.0.0.1:19090/` | 显示设置面板；地址框预填 `http://127.0.0.1:19090` |
| 2 | 首次进入不填密码直接点「创建新房间」 | 提示「请输入密码」，不发起连接 |
| 3 | 刷新页面（此前已成功连接过） | 300ms 后自动 `resume`，直接进入聊天页，**无需重新输入密码**；历史消息完整显示 |

### S1 房间初始化与配对

| 步骤 | 操作 | 预期 |
|---|---|---|
| 4 | 清空 `.\data` 后重启服务；窗口 A 输入 `secret1234` →「创建新房间」 | 状态栏「身份 A · 对端 B 离线」；聊天区空 |
| 5 | 窗口 B（无痕）输入同一密码 →「已有房间，加入」 | 状态栏「身份 B · 对端 A 在线」 |
| 6 | 观察窗口 A（不刷新） | 2s 内变为「身份 A · 对端 B 在线」（presence 推送） |
| 7 | 再开一个窗口 C，同一密码再点「创建新房间」 | 被拒绝：`auth.result.success=false`，提示「房间已初始化，请使用 login」 |
| 8 | 窗口 C 改为点「已有房间，加入」，身份下拉保持「自动分配」 | 被拒绝，提示「房间内 A、B 都已有设备，请显式指定 desired_role=A 或 B」 |
| 9 | 窗口 C 身份下拉改选「角色 A」，再点「已有房间，加入」 | 进入成功，状态栏「身份 A · 对端 B 在线」 |

### S2 在线互发与回执

| 步骤 | 操作 | 预期 |
|---|---|---|
| 10 | A 发「hello-b」 | 气泡立即在右侧（乐观渲染），随后 meta 中 `#seq` 从 1 开始递增 |
| 11 | 观察 B | 左侧出现「hello-b」，标注「角色 A」与 seq；A 侧不出现重复气泡 |
| 12 | B 发「hello-a」 | A 收到，标注「角色 B」 |
| 13 | 连续快速发送 5 条（Enter 连点） | 5 条全部出现且 seq 连续递增，无丢失、无重复、无乱序 |

### S3 同角色多设备（本轮新增行为）

| 步骤 | 操作 | 预期 |
|---|---|---|
| 14 | 新开无痕窗口 C，密码 + 身份下拉选「角色 A」→「已有房间，加入」 | C 进入，状态栏「身份 A · 对端 B 在线」 |
| 15 | B 发「to-all-As」 | 窗口 A **和** 窗口 C 都收到，seq 相同 |
| 16 | **A1（窗口 A）发「from-A1」** | B 收到；**A2（窗口 C）也收到**；**A1 自己不重复显示这条**（乐观气泡只出现一次） |
| 17 | 关闭窗口 C | A 状态栏不变（B 仍在线；presence 只在该角色连接数 1→0 时广播） |

### S4 图片消息与上传边界

| 步骤 | 操作 | 预期 |
|---|---|---|
| 18 | A 点 🖼 发送一张 <10MB 真图片 | A 侧图片气泡渲染，meta 显示尺寸/大小；B 自动加载，点击可看原图 |
| 19 | A 发送 >10MB 的图片 | 上传被整体拒绝（HTTP 400，错误信息含「大小上限」），聊天区不出现占位气泡 |
| 20 | 把 `.txt` 改名 `.png` 发送 | 被拒绝（服务端按内容嗅探，非图片 400） |
| 21 | 发送后检查服务端 `data\files\` | 只有合法文件；临时目录无残留（超限上传不落盘） |

### S5 离线补投、刷新与切换服务器/身份

| 步骤 | 操作 | 预期 |
|---|---|---|
| 22 | 关闭 B 窗口 | A 状态栏 2s 内变「对端 B 离线」 |
| 23 | A 连发「offline-1」「offline-2」 | 两条正常发送并回填 seq |
| 24 | 重开 B（无痕）→ 加入 | B 自动同步，历史全部显示（含图片），**每条仅一次** |
| 25 | 刷新窗口 A（F5） | A 消息区清空 → 自动 resume → 全量同步后**历史完整、无重复** |
| 26 | 窗口 A 点右上「设置」→ 把地址改为另一台服务器（或另一个端口的新实例）→「已有房间，加入」 | 消息区被清空，**不会出现上一台服务器的消息**；新服务器按自己的历史从 0 开始 |
| 27 | 在步骤 26 中，新服务器分配给该设备的角色与旧角色不同（A↔B） | 消息区同样被清空，不串上一身份的消息；状态栏角色正确 |

### S6 断线与重连竞态

| 步骤 | 操作 | 预期 |
|---|---|---|
| 28 | 停止服务端进程（不删 data） | 两窗口状态栏变「连接断开」，自动回到设置面板，凭据不清除 |
| 29 | 重新启动服务 | 刷新窗口 → 自动 resume，历史完整 |
| 30 | 在设置面板快速连续点两次「已有房间，加入」（间隔 <0.5s） | 最终停留在聊天页，**不会被旧 socket 的 onclose 踢回设置页**；消息不重复 |
| 31 | 断网/恢复（可选：禁用网卡 10s） | 恢复后重连成功，状态栏正确，历史不重复 |

### S7 resume 与 token 吊销（DevTools）

> 目的：验证 `resume` 不再"只验签名"，旧 token 换发后立即失效。
> 以下脚本在窗口 A 的 DevTools Console 执行。

```js
// 读取当前凭据并尝试合法 resume
const cfg = JSON.parse(localStorage.getItem('fish.cfg'));
const ws = new WebSocket('ws://127.0.0.1:19090/ws');
ws.onopen = () => ws.send(JSON.stringify({
  v: 1, type: 'auth',
  data: { action: 'resume', device_id: cfg.deviceID, token: cfg.token }
}));
ws.onmessage = e => console.log('resume:', e.data);
```

| 步骤 | 操作 | 预期 |
|---|---|---|
| 32 | 执行上面的脚本 | `auth.result` 返回 `success:true`、`role` 与当前一致、`device_id` 回填 |
| 33 | 把 `device_id` 改成 `"someone-else"` 再执行 | `success:false`，错误「device_id 与 token 不匹配」 |
| 34 | 把 `token` 改成 `"bad"` 再执行 | `success:false`，错误「token 无效」 |
| 35 | 用同一 `device_id` + 密码走 `login` 换发新 token（见下方脚本），再用**旧** token 执行 resume | 旧 token `success:false`（「token 无效」）；新 token 可用 |
| 36 | 用旧 token 请求 `GET /api/files/xxx?token=<旧token>` | 401（文件接口与 resume 共享吊销语义） |

> 第二个脚本会把新 token 写回 `localStorage`；**当前页面**仍持有旧 token（后续加载图片会 401），
> 测完请刷新页面，让自动 resume 使用新 token。

```js
// 用同一设备重新 login，换发 token（会写回 localStorage）
const cfg2 = JSON.parse(localStorage.getItem('fish.cfg'));
const ws2 = new WebSocket('ws://127.0.0.1:19090/ws');
ws2.onopen = () => ws2.send(JSON.stringify({
  v: 1, type: 'auth',
  data: { action: 'login', device_id: cfg2.deviceID, password: 'secret1234' }
}));
ws2.onmessage = e => {
  console.log('login:', e.data);
  const d = JSON.parse(e.data).data;
  if (d.success) {
    localStorage.setItem('fish.cfg', JSON.stringify({ ...cfg2, token: d.token, role: d.role }));
  }
};
```

### S8 登录失败限流

| 步骤 | 操作 | 预期 |
|---|---|---|
| 37 | **先重启服务清空限流计数**（S7 的失败尝试也会计数）；新无痕窗口，输入**错误**密码点「已有房间，加入」，重复 10 次 | 前 10 次均提示「密码错误」 |
| 38 | 第 11 次输入**正确**密码 | 仍被拒绝，提示「登录失败次数过多，请稍后再试」（限流按来源 IP，5 分钟窗口） |
| 39 | 等待 5 分钟（或重启服务进程）后再次正确密码登录 | 恢复正常 |

> 注意：限流键是客户端 IP。触发后本机所有窗口在窗口期内都会被拒绝，测试完请等待或重启服务。

### S9 协议版本校验

在 DevTools Console 执行：

```js
const cfg3 = JSON.parse(localStorage.getItem('fish.cfg'));
const ws3 = new WebSocket('ws://127.0.0.1:19090/ws');
ws3.onopen = () => ws3.send(JSON.stringify({
  v: 1, type: 'auth',
  data: { action: 'resume', device_id: cfg3.deviceID, token: cfg3.token }
}));
ws3.onmessage = e => {
  const f = JSON.parse(e.data);
  console.log(f);
  if (f.type === 'auth.result' && f.data.success) {
    // 故意发送 v=2 的业务帧
    ws3.send(JSON.stringify({ v: 2, type: 'msg.send', id: 'v2', data: { kind: 'text', body: 'x' } }));
  }
};
```

| 步骤 | 预期 |
|---|---|
| 40 | 收到 `error` 帧，`data.code = 4003`、`message = 不支持的协议版本: 2`；消息未落库（聊天区不出现） |

### S10 边界与安全

| 步骤 | 操作 | 预期 |
|---|---|---|
| 41 | 用 UI 粘贴并发送 8193 字节文本（>8KB） | 服务端返回 `error 4003`；UI 移除该乐观气泡并显示错误提示，页面不崩溃 |
| 42 | 未带 token 访问 `/api/files/任意?token=` | 401 |
| 43 | 访问 `/api/files/..%2f..%2fconfig.json?token=<有效>` | 400（非法 ID，目录穿越被拒） |
| 44 | 访问不存在的合法格式 ID（32 位 hex） | 404 |
| 45 | `curl.exe -s http://127.0.0.1:19090/healthz`（PowerShell 中 `curl` 是别名，需写 `curl.exe`） | `{"status":"ok"}` |
| 46 | 首帧不发 `auth`（Console 连上后直接发 `ping`） | `auth.result.success=false`，提示「首帧必须为 auth」 |

### S11 重启持久化

| 步骤 | 操作 | 预期 |
|---|---|---|
| 47 | 停止服务 → 重新启动（**不要删 data**） | 日志「当前最大 seq=N」（N>0）；不重新生成 JWT key；房间密码仍有效 |
| 48 | 刷新 A、B 窗口 | 全部历史完整恢复，自动 resume，无需重新输入密码 |
| 49 | 删除 `data\jwt.key` 后重启，再刷新浏览器 | 旧 token resume 失败 → 自动清除本地凭据并回到设置页，需重新输入密码 |

### S12 SDK 冒烟（命令行）

```powershell
go test ./client/ -run 'TestInitAndLogin|TestSendAndReceive|TestSync|TestDeliveredAck' -count=1 -v
go test ./client/ -run 'TestReconnectAfterClose|TestCallbackCanSend|TestConcurrentMultiDeviceSend' -count=1 -v
```

预期：全部 PASS。关键含义：
- `TestReconnectAfterClose`：重连后旧连接收尾不影响新连接，从未连接时 `Close()` 不 panic
- `TestCallbackCanSend`：事件回调内调用 `SendText` 不会因等待 ack 超时
- `TestConcurrentMultiDeviceSend`：同角色多设备并发发送，回执不串扰

---

## 四、部署与 CI 验证

部署细节见 [deploy/README.md](file:///c:/Documents/Projects/Fish/deploy/README.md)。改动合并到 `main` 后 GitHub Actions 自动执行：

| 步骤 | 检查点 |
|---|---|
| Checkout / Set up Go | 使用 `go-version-file: go.mod`，不再写死版本 |
| Build | 日志含 `Binary size: N bytes`；`CGO_ENABLED=0` |
| Upload binary | `scp-action` 上传成功，服务器出现 `/tmp/fish-upload/fish-server` |
| 远程脚本 | 日志依次出现 `+ systemd 单元就绪`、`+ 二进制已替换（旧版本已备份为 fish-server.bak）`、`✓ fish 启动成功`、`{"status":"ok"}` |
| 失败回滚 | 若启动失败，日志含 `✗ fish 启动失败，自动回滚...`，旧二进制恢复并启动 |
| 幂等性 | 连续触发两次部署：第二次不重复创建用户，服务正常重启，数据（`/opt/fish/data`）不丢 |

服务器侧手工复核：

```bash
systemctl is-active fish
systemctl status fish --no-pager | head -15
curl -fsS http://127.0.0.1:19090/healthz
ls -l /opt/fish/fish-server /opt/fish/fish-server.bak
journalctl -u fish -n 30 --no-pager
```

---

## 五、本轮修复的专项回归清单

| # | 已修复缺陷 | 验证方式 |
|---|---|---|
| 1 | SDK 先发送后注册 ack 等待器 → 偶发超时 | 自动化 `client` 全部用例；S2 连发 |
| 2 | 重连后 `closeOnce` 失效、字段竞争 | `TestReconnectAfterClose`；S6 步骤 28–31 |
| 3 | 上传超限被静默截断 + 临时文件泄漏 | `TestFileUpload_RejectOversize`；S4 步骤 19、21 |
| 4 | Web 切换服务器/身份未重置游标 | S5 步骤 26–27 |
| 5 | 零配置启动（config.json 非必需） | 一.2 四种启动方式；日志「配置文件: config.json」 |
| 6 | 同角色其他设备收不到自己发出的消息 | `TestWS_SameRoleEcho`；S3 步骤 16 |
| 7 | 回调内调用 Send/Sync 必然超时（事件分发阻塞 readLoop） | `TestCallbackCanSend`；S12 |
| 8 | `genRef` 同纳秒碰撞 | `TestConcurrentMultiDeviceSend`；S2 步骤 13 |
| 9 | `resume` 不校验 device_id/设备/token | `TestWS_ResumeValidation`、`TestResume_Validation`；S7 |
| 10 | 业务帧版本不校验 | `TestWS_FrameVersionRejected`；S9 |
| 11 | 并发 `init` 双 A | `TestInit_ConcurrentOnlyOneSucceeds` |
| 12 | Web 旧 socket `onclose` 误踢回设置页 / `syncing` 卡死 | S6 步骤 30；S7 步骤 32 |
| 13 | 登录无失败限流 | `TestWS_AuthRateLimited`；S8 |
| 14 | 文件接口旧 token 仍可用 | S7 步骤 36 |
| 15 | 部署 workflow scp 参数无效 / go 版本不一致 | 四、部署与 CI 验证 |

---

## 六、验收标准（全部满足即通过）

### 启动与配置
- [ ] AC-1 空目录零配置可启动；不生成 config.json；`jwt.key` 持久化
- [ ] AC-2 配置优先级正确：默认 < config.json < `FISH_*` < 命令行；非法 env 数值报错退出

### 鉴权与配对
- [ ] AC-3 首台 `init` 成为 A；重复 init 被拒；并发 init 只有一个成功
- [ ] AC-4 新设备自动分配 B；A/B 都已存在时必须显式 `desired_role`
- [ ] AC-5 错误密码被拒；未初始化房间 login 被拒
- [ ] AC-6 `resume` 校验 device_id、设备存在性、token 是否为当前 token；重登后旧 token 失效
- [ ] AC-7 同一 IP 连续 10 次失败后限流，5 分钟后恢复

### 消息链路
- [ ] AC-8 A↔B 文本实时到达（本机 <1s），seq 单调递增，无重复
- [ ] AC-9 对端离线期间消息补投顺序正确、无重复
- [ ] AC-10 同角色多设备：对端消息广播到该角色所有设备；**本角色其他设备能收到自己角色发出的消息，发送设备自身不重复**
- [ ] AC-11 对端上/下线 2s 内状态栏更新（presence）
- [ ] AC-12 `msg.send → msg.ack → msg.delivered` 关联正确，多设备并发无串扰

### 文件
- [ ] AC-13 图片上传/下载/类型嗅探正确；目录穿越 400、不存在 404、未授权 401
- [ ] AC-14 超过 10MB 整体拒绝（不截断、不落盘、无临时文件残留）

### 客户端（Web / SDK）
- [ ] AC-15 Web：创建/加入房间进入聊天页；仍是身份 A/B 正确
- [ ] AC-16 Web：localStorage 持久化 device_id/token/role，刷新自动 resume
- [ ] AC-17 Web：刷新后历史完整、每条仅一次；**切换服务器/身份后消息区清空且不串消息**
- [ ] AC-18 Web：断网/重启后重连成功；快速重复连接不会被旧 socket 踢回设置页
- [ ] AC-19 SDK：init/login/resume 三种动作、sync、图片、回执、重连、回调内发送全部可用

### 协议与安全
- [ ] AC-20 首帧必须为 `auth`；`auth.result` 成功/失败语义清晰
- [ ] AC-21 业务帧 `v != 1` 返回 `error 4003`
- [ ] AC-22 客户端按 seq 幂等去重
- [ ] AC-23 自动化测试全绿（55 用例）+ `go vet` 无告警

### 部署
- [ ] AC-24 CI 部署成功日志与回滚日志符合第四节
- [ ] AC-25 `curl /healthz` 返回 `{"status":"ok"}`；重启后历史与房间密码保留

---

## 七、常见问题

| 现象 | 原因 / 处理 |
|---|---|
| 密码忘了想重开房间 | 停服务 → 删 `data\fish.db` 与 `data\jwt.key` → 重启（历史与文件也一并失效） |
| 测试时"修复没生效" | 19090 上可能是旧进程：`netstat -ano \| findstr :19090` 确认并重启新二进制 |
| 一直提示「登录失败次数过多」 | 触发了 S8 限流：等待 5 分钟、重启服务，或换来源 IP |
| 刷新后要重新输密码 | `localStorage` 被清（无痕窗口关闭即清）、或服务端 `jwt.key` 被删除导致 token 失效 |
| 两个窗口身份串号 | 浏览器同域共享 `localStorage`；用普通 + 无痕窗口，或先点「设置」后清空站点数据 |
| 图片上传 400 | 非真实图片（服务端嗅探魔数，改扩展名无效）或超过大小上限 |
| 上传大图后气泡不出现 | 预期：超限整体拒绝；看页面顶部错误提示与 HTTP 400 |
| 发送超长文本报 4003 | 单帧文本上限 8KB（`FISH_MAX_TEXT_BYTES` 可调） |
| 端口被占用 | PowerShell 查 PID 后 `Stop-Process`；或换 `-listen :1909x` |
| 改了环境变量端口没变 | 命令行参数优先级最高；systemd 中显式 `-listen`/`FISH_LISTEN` 需同步修改 |
