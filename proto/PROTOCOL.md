# Fish 私有消息系统 — 协议规范 v1

单房间双端（A↔B）私信中转协议。协议与平台解耦：任何客户端（手机/桌面/agent）按本文档实现即可接入。

## 1. 概览

- 服务端职责：鉴权、消息路由、存储。不含业务逻辑。
- 配对方式：**动态配对**，无预置 token、无注册后台。
  - 首台设备用 `init`（设置房间密码）成为角色 **A**；
  - 第二台设备用 `login` 成为角色 **B**；
  - 之后的新设备登录时按已有设备自动分配；A、B 都已有设备时必须显式指定 `desired_role`。
- 同一角色允许多台设备同时在线；消息投递给该角色的全部在线连接（发送方连接除外，见 §4.2）。
- 传输：
  - WebSocket `GET /ws`：帧内鉴权、实时收发、回执、增量同步。
  - HTTP：图片上传/下载、健康检查。

## 2. 鉴权

鉴权通过 **WS 帧内握手** 完成，HTTP 层不做鉴权。

### 2.1 WS 握手流程

1. `GET /ws` 升级为 WebSocket。
2. 客户端发送首个帧，`type` 必须为 `auth`：

```json
{
  "v": 1, "type": "auth", "ts": 1760000000000,
  "data": {
    "action": "init | login | resume",
    "device_id": "d3f1...",
    "password": "secret1234",
    "desired_role": "A",
    "token": "<JWT，resume 专用>"
  }
}
```

3. 服务端回 `auth.result`（无论成功失败都只回这一帧）：

```json
// 成功
{"v":1,"type":"auth.result","data":{
  "success":true,"role":"A","device_id":"d3f1...","token":"<JWT>",
  "peer_online":false,"last_seq":42}}
// 失败
{"v":1,"type":"auth.result","data":{"success":false,"error":"密码错误"}}
```

### 2.2 action 语义

| action | 说明 |
|---|---|
| `init` | 用 `password` 初始化房间并注册为 A。房间已存在则失败（`房间已初始化，请使用 login`）。密码长度 4–64。 |
| `login` | 用 `password` 登录。已注册的 `device_id` 复用原角色并换发新 token；新设备按规则分配角色（A、B 都已存在时必须给 `desired_role`）。 |
| `resume` | 用 `token` 恢复连接，无需密码。`device_id` 可留空（服务端从 token 解析）；若填写则必须与 token 内设备一致。 |

`resume` 的额外校验：

- token 必须签名有效且未过期（默认 365 天）；
- 设备必须仍已注册，且角色与 token 一致；
- token 必须仍是该设备**当前** token —— 同一设备重新 `login` 会换发新 token，旧 token 立即失效。

### 2.3 HTTP 鉴权

`Authorization: Bearer <token>`；图片下载额外支持 `?token=`（便于 `<img>` 直接引用）。
token 无效返回 401。

## 3. 帧结构（信封）

```json
{"v":1, "type":"msg.send", "id":"req-1", "ts":1760000000000, "data":{ }}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| v | int | 协议版本，当前必须为 `1` |
| type | string | 帧类型，见 §4 |
| id | string | 客户端生成的请求关联 id（响应帧原样回传），服务端主动推送帧无 id |
| ts | int64 | 发送方 Unix 毫秒时间戳 |
| data | object | 各帧类型自定义载荷 |

业务帧 `v != 1` 时服务端返回 `error{code:4003}` 并丢弃该帧。

## 4. 帧类型

### 4.1 客户端 → 服务端

| type | data | 说明 |
|---|---|---|
| `auth` | 见 §2.1 | 仅作为连接首帧 |
| `msg.send` | `{kind:"text", body:"..."}` 或 `{kind:"image", file_id:"...", w,h,size}` | 发消息 |
| `msg.ack` | `{seq:123}` | 接收端确认已收到该消息（送达回执） |
| `sync` | `{after:N, limit:100}` | 拉取 `seq > N` 的历史消息，limit ≤ 200（默认 100） |
| `ping` | `{}` | 心跳 |

### 4.2 服务端 → 客户端

| type | data | 说明 |
|---|---|---|
| `auth.result` | 见 §2.1 | 鉴权结果（连接首帧） |
| `msg.ack` | `{ref:"req-1", seq:101, ts:...}` | 服务端已收到并持久化 `msg.send`（ref 为请求帧 id） |
| `msg.new` | `{seq:102, from:"B", kind:"text", body:"...", ts:...}` | 投递消息，推送对端全部在线连接 + 发送方同角色的其他在线连接 |
| `msg.delivered` | `{seq:102}` | 对端已通过 `msg.ack` 确认（发送方收到的送达回执） |
| `sync.done` | `{msgs:[msg.new 载荷...], has_more:false}` | `sync` 响应，消息按 seq 升序 |
| `presence` | `{peer:"B", online:false}` | 对端在线状态变化推送（对端首条连接建立/末条连接断开时） |
| `pong` | `{}` | 心跳应答 |
| `error` | `{code:4003, message:"...", ref:"req-1"}` | 错误，ref 可选 |

### 4.3 错误码

| code | 含义 |
|---|---|
| 4003 | 参数错误（缺字段/超限/协议版本不支持） |
| 4004 | 不支持的帧类型或消息 kind |
| 5000 | 服务端内部错误 |

> 鉴权失败通过 `auth.result{success:false,error}` 表达，不使用 `error` 帧。

## 5. seq 语义（核心）

- 服务端为每条消息分配**全会话单调递增 seq**（从 1 开始）。
- 客户端本地记录已收到的最大 seq（游标）：
  - 上线后发 `sync {after: 游标}` 补拉离线消息；
  - 在线期间被动接收 `msg.new`。
- 按 seq 幂等去重：客户端收到 seq ≤ 游标的消息直接丢弃。
- 投递顺序保证：按 seq 升序投递；并发 `msg.send` 的落库与分配 seq 由服务端串行化。

> **重要**：`auth.result.last_seq` 是**服务端**当前最大 seq，不是客户端本地游标。
> 客户端必须用自己记录的本地游标作为 `sync.after`，**不得**用 `last_seq` 覆盖本地游标，
> 否则首次连接（本地游标为 0）或换设备时将丢失全部历史消息。
> `last_seq` 仅用于判断「本地是否落后」以决定是否发起 `sync`。

> **游标作用域**：客户端游标只能与「本地已渲染/已存储的消息」绑定。
> 若客户端只保存游标、不持久化消息内容（如浏览器刷新导致界面清空），
> 游标必须同时归零，重连时从 `after:0` 全量拉取；否则会出现
> 「游标显示已收到 N 条，但界面为空，只能看到新补投消息」的缺陷。
> 需要跨会话保存消息的客户端，应把游标与消息一起持久化（游标不得单独持久化）。

## 6. 消息时序（正常路径）

```
A                                服务端                                B
│ msg.send(id:req-1,text"hi") ──► │                                    │
│                                 │ 持久化, 分配 seq=101               │
│ ◄── msg.ack(ref:req-1,101) ──── │                                    │
│                                 │ ──► msg.new(seq:101,from:A) ──►    │ (B 在线)
│                                 │                          B 处理    │
│                                 │ ◄──────────────── msg.ack(101) ── │
│ ◄── msg.delivered(101) ──────── │                                    │
```

- B 离线时：消息已落库，B 上线后 `sync` 补投；`msg.delivered` 在 B 补投后回执时补发（A 重连后 sync 不含回执，本期回执为尽力而为，不持久化回执状态）。
- 同角色多设备：A1 发送后，除对端 B 外，同角色的 A2 也会收到同一条 `msg.new`（发送方 A1 自身不重复收）。

## 7. 图片消息

1. `POST /api/files`（multipart/form-data，字段名 `file`，Bearer 鉴权）→ 返回 `{"file_id":"...","size":123}`
2. WS 发 `msg.send {kind:"image", file_id, w, h, size}`（w/h 可选）
3. 接收端展示：`GET /api/files/{file_id}?token=xxx`

限制：单文件 ≤ 10MB（含超限校验，不会静默截断）；类型按内容嗅探，仅 `image/jpeg|png|gif|webp`。

## 8. 心跳与连接管理

- 客户端每 25s 发 `ping`，服务端立即回 `pong`。
- 服务端 90s 未从连接读到任何帧则主动断开；客户端读取超时 120s。
- 断线重连由客户端负责：`resume` → `auth.result` → `sync` 补投。
- 服务端在写队列拥塞（队列满）时主动断开连接，避免消息静默堆积。

## 9. 容量限制（2C2G 目标）

| 项 | 限制 |
|---|---|
| 文本消息单帧 | ≤ 8 KB（`MaxTextBytes` 可配） |
| 图片单文件 | ≤ 10 MB（`MaxFileBytes` 可配） |
| WS 单帧上限 | 1 MB（含图片引用元数据足够） |
| sync 单次 limit | ≤ 200 |
| 历史保留 | 永久（可用 SQL 手工清理，见部署文档） |

## 10. 版本

- `v=1` 为当前版本。业务帧 `v != 1` → `error 4003`。
- 历史版本说明：早期设计中的 `ready` 首帧、Bearer/?token 的 WS 鉴权、固定身份 `a`/`b` 均已废弃，
  首帧信息统一由 `auth.result` 下发。
