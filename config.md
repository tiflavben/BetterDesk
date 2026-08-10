# BetterDesk 项目状态文档（config.md）

> 本文档供后续开发续接使用。**禁止写入任何凭据**（API key、数据库密码、PAT、SSH 私钥、管理员密码、会话 cookie）——一律以 `[REDACTED]` 表示。
> 最后更新：2026-08-10（上游合并 + 用户自服务设备管理后）

---

## 1. 项目概述

本项目是 RustDesk/BetterDesk 的 fork 部署，在 fork 基础上实现：

- **真实跨机器 Relay 集群**（1 主控 + 2 relay-only）
- **共享 PostgreSQL ticket store**、跨 relay 认领、防重放、故障切换、并发、双向流量计量
- **Relay 实时 telemetry**（在线、CPU/RAM、5s 带宽、活跃会话、累计字节）
- **用户资源管理**（设备数/设备限额/流量配额与已用/到期时间，合同驱动）
- **管理员 vs 普通用户严格隔离**（后端权限 + 前端菜单双层控制）
- **普通用户设备自服务**（查看/封禁/解除封禁/删除/恢复自己的设备）

## 2. 固定拓扑与地址

| 主机 | 角色 | 地址 | systemd 服务 | 端口 |
|---|---|---|---|---|
| 101 | 主控（Signal + API + 面板 + PostgreSQL） | 192.168.1.101 | `betterdesk-master.service`、`betterdesk-console.service` | 21114 (API/Signal)、21115 (Signal TCP)、5000 (面板)、5432 (PG) |
| 102 | Relay 1（relay-only） | 192.168.1.102 | `betterdesk-relay1.service` | 21117 |
| 103 | Relay 2（relay-only） | 192.168.1.103 | `betterdesk-relay2.service` | 21117 |

- 面板 URL：`http://192.168.1.101:5000`（管理员 `admin`，密码 `[REDACTED]`）
- 测试普通用户：`517532265` / `[REDACTED]`（viewer 角色，用于隔离验收；真实业务使用中）

### 关键配置（101 主控）

- `RELAY_SERVERS=192.168.1.102:21117,192.168.1.103:21117` —— **必须所有 Signal 实例完全一致且顺序一致**（Relay 哈希分配依赖顺序）
- `RELAY_TICKET_STORE=db` —— relay-only 必需，共享主控 PostgreSQL 作 ticket store
- `ENROLLMENT_MODE=open`
- Go 服务数据目录：`/etc/betterdesk/`（id_ed25519、.api_key）
- 面板服务环境：`KEYS_PATH=/etc/betterdesk`、`BETTERDESK_API_URL=http://127.0.0.1:21114/api`、`DATA_DIR=/opt/betterdesk-console/data`

## 3. 数据库与数据来源

| 库 | 位置 | 用途 |
|---|---|---|
| PostgreSQL `betterdesk` | 101:5432 | **权威**：peers、billing_contracts/packages/sessions、relay_tickets、relay_traffic、relay_heartbeat、users |
| `auth.db` | `/opt/betterdesk-console/data/auth.db` | 面板用户/会话/settings（**Relay 节点配置 `scaling_relay_nodes` 必须写这里**，写 `db_v2.sqlite3` 无效） |
| `db_v2.sqlite3` | `/opt/betterdesk-console/data/db_v2.sqlite3` | 面板业务数据（设备分组/文件夹/审计等），**不是** scaling 节点来源 |

- 面板用户与 Go 用户是**两套用户库**：面板登录走 auth.db，RustDesk 客户端/Go API 登录走 PG `users` 表。改密码需**两侧分别改**（面板 `/api/users/:id/reset-password` + Go `PUT /api/users/:id`）。
- 设备归属：`peers."user"`（PG），Go API 序列化为 `username` 字段。

## 4. Git 状态

- 工作区：`F:\betterdesk`，分支 `dev`（推送 `fork` = tiflavben/BetterDesk）
- Remote：`origin` = UNITRONIX/BetterDesk（**上游**，主分支 `dev`）；`fork` = tiflavben/BetterDesk
- 最近提交链（dev）：
  - `f4d4e5c` Merge origin/dev（**上游 12 个新提交已合并**：agent Wails UI、fleet org 过滤修复、attestation 对比度、版本 bump 至 3.5.29）
  - `10cc31d` fix(billing): user-scoped contracts resolved & enforced
  - `423da9b` style(dashboard): 订阅卡主题色
  - `ff6192e` feat(dashboard): 我的订阅卡 + 隐藏 UX 3.5 切换
  - `0550f7f` feat(ui): 管理员/普通用户 UI 隔离
  - 更早：`2e2fe18` 用户资源管理、`add0c6a`/`d941ca3`/`93beda3`/`c244eff`/`1c4b80c` Relay telemetry 链
- **GitHub push 注意事项**：本机 git 全局代理已失效，push 必须
  `env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY git -c http.proxy= -c https.proxy= push https://<token>:<token>@github.com/tiflavben/BetterDesk.git dev`（token 文件 `C:\Users\xoon\rdgen_ghbearer_token.txt`，仅本地，禁入 git/文档）
- 敏感文件（禁读/禁提交/禁记录内容）：`github_pat.txt`、`F:\betterdesk\bin\` 下构建产物、任何 token/私钥文件

## 5. 权限架构（管理员 vs 普通用户）

### 角色

- 管理员类：`admin` / `super_admin` / `server_admin` / `global_admin`（`isAdminUser`/`requireAdmin`）
- 普通用户类：`viewer` / `operator` / `pro`（viewer 权限：device.view、audit.view、metrics.view、cdap.view、chat.access）

### 后端（双层控制第一层）

- 页面路由守卫：`middleware/auth.js` 的 `requireAuth` / `requirePermission(perm)` / `requireRole` / `requireAdmin`
- 管理页面（inventory/tickets/automation/network/activity/reports/cdap/organizations/security-audit）→ `requireAdmin` 或对应权限（普通用户直接访问返回 403）
- 设备操作（ban/unban/delete/restore）：`requireAuth` + **端点内 `rejectIfDeviceOutOfScope`**（管理员全范围；普通用户仅自己 scope 内设备，越权 403）
- 设备列表 scope：`getVisibleDevicesForRequest` → `getDeviceScopeForUser`（admin=null 全量；普通用户按 device groups/peer grants/**owner 设备**（`peers."user"==username`）过滤）
- 面板 `/api/me/contract`：任何登录用户只读**自己**的合同摘要（到期/流量/设备限额）
- Go API：`requirePermission`（`auth.Perm*`）按权限保护；`/api/peers` 对非管理员按 org/ACL 过滤

### 前端（双层控制第二层，仅隐藏不兜底）

- `server.js` 注入 `res.locals.isAdminUser`（模板变量）
- `sidebar.ejs`：management/tools/system rail 按钮仅 `isAdminUser`；navbar 的 UX 3.5 切换按钮仅管理员
- `dashboard.ejs`：已封禁卡、服务器状态区仅管理员（模板字符串内三元 `${isAdminUser ? ... : ''}`）；普通用户显示"我的订阅"卡（到期/流量/设备限额）
- `settings.ejs`：品牌 tab、审计日志区仅管理员；`settings.js` 非管理员时移除品牌 tab
- `devices.ejs`：已封禁筛选按钮仅管理员

## 6. 用户资源管理（合同模型）

- `billing_contracts`：`target_type`（org/user/device/folder/group）、`target_key`、`quota_bytes`（0=不限）、`used_bytes`、`device_limit`（0=不限）、`valid_from`/`valid_until`、`minutes`、`status`（active/expired/suspended）
- 用户合同：`target_type='user'`、`target_key=<username>`；**必须引用真实存在的 `billing_packages`**（否则创建 500）
- 解析链（`billing/resolver.go` `ResolveContractForDevice`）：device > folder > device_group > **user** > org
- 强制点（`billing/service.go` `CheckConnection`）：未开始/过期/suspended/流量耗尽/分钟耗尽/`device_limit_reached`（在线设备数 >= limit）均拒绝
- 流量计量：relay 双向 `countingConn` 流式累计 → `relay_traffic` 表（relay-only 写 PG）→ 主控 billing 消费 → 合同 `used_bytes`
- `GET /api/users` 返回每个用户 `device_count` + `contract` 摘要；面板 `/users` 页按 username 合并（Go 权威 + 面板资料）

## 7. Relay Telemetry

- Go：`GET /api/scaling/relays`（API key + `server.config` 权限）→ TCP 在线探测 + `relay_heartbeat`（5s 采样：CPU%、RAM%、`bandwidth_mbps=(ΔTotalBytes*8/1e6/5)`、active_sessions、total_bytes）
- 面板 `/scaling` 页：`routes/scaling.routes.js` 代理真实 telemetry，`public/js/scaling.js` 10s 轮询
- 面板 Relay 节点清单来源：`auth.db.settings.scaling_relay_nodes`（与 Go `RELAY_SERVERS` 独立）

## 8. 验证基线（已实测）

- 集群：跨机票据认领、双向转发、防重放、Relay 故障切换、5 并发会话、双向流量 4,194,304 bytes ✓
- billing：`go test ./billing/ ./db/` 全绿；真实合同到期/限额/流量 E2E ✓
- Relay telemetry：浏览器 `/scaling` 显示 2 节点 online、CPU/RAM、活跃会话、Mbps ✓
- 用户资源：517532265 合同（限额 3、500MB、2026-09-09）面板显示 ✓；设备绑定后 `device_count` 0→1 ✓
- UI 隔离：viewer 登录实测（rail 仅仪表板/设置、banned 卡/服务器状态/审计/品牌隐藏、`/inventory` 403）✓
- 上游合并后：`go test ./...` 全绿、Go/Node 构建通过、已部署三机、面板 API 冒烟 200 ✓

## 9. 进行中 / 待办

- [ ] **设备页普通用户显示 0 台（排查中）**：上游合并后 `/api/devices` 返回 `data.devices` 嵌套，`devices.js` 已兼容（`response.devices || response.data?.devices`）；浏览器缓存 `?v=` 旧 JS 疑似主因——重启面板换时间戳；子代理正在最终定位（含 scope owner 增强验证：1300228927 应显示、486608902 不应显示）
- [ ] 全面 bug 扫描（前端路由/权限 + Go 服务端）——子代理进行中，结果待合入
- [ ] 浏览器级 UI 登录联调（管理员与普通用户双视角完整走查，`http://192.168.1.101:5000`）
- [ ] 上游合并后需复验：Relay 心跳/流量计量链路、billing E2E（合并可能影响 Go 依赖）
- [ ] 用户合同创建 UI 应明确要求 package 或自动选择合法 package（避免 500）
- [ ] 测试用户 517532265 及其合同/package 清理与否待用户确认（真实业务使用中）
- [ ] LAN 直连流量不经过 relay（RustDesk 架构），不计入合同流量——如需计费需另行设计
- [ ] `git status` 中 `bin/`、`github_pat.txt` 未跟踪——确认 gitignore 策略

## 10. 部署速查

```bash
# 构建 Go（Windows 本机交叉编译）
cd F:\betterdesk\betterdesk-server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o F:/betterdesk/bin/betterdesk-server-linux-amd64 .

# 部署主控 101
ssh root@192.168.1.101 'systemctl stop betterdesk-master; rm -f /opt/betterdesk/betterdesk-server'
scp F:/betterdesk/bin/betterdesk-server-linux-amd64 root@192.168.1.101:/opt/betterdesk/betterdesk-server
ssh root@192.168.1.101 'chmod +x /opt/betterdesk/betterdesk-server && systemctl start betterdesk-master'

# 部署面板 101
cd F:\betterdesk\web-nodejs
scp -r views/ routes/ services/ public/js/ public/css/ lang/ root@192.168.1.101:/opt/betterdesk-console/
ssh root@192.168.1.101 'systemctl restart betterdesk-console'

# 部署 Relay（102/103）：构建后 scp 到各 relay 并重启对应服务
# 验证
ssh root@192.168.1.101 'systemctl is-active betterdesk-master betterdesk-console'
curl -s -H "X-API-Key: $(ssh root@192.168.1.101 'cat /etc/betterdesk/.api_key')" http://192.168.1.101:21114/api/users
```

## 11. 常见坑

1. **EJS 模板风格**：`dashboard.ejs`/`settings.ejs`/`devices.ejs` 是 `<%- include()` 模板**字符串**结构——内部用 `${...}` 插值，**不能插入 `<% %>` EJS 标签**（编译报 "matching close tag"）；条件渲染用模板字符串内三元 `${cond ? `...` : ''}`
2. **两套用户库密码不同步**：面板重置密码 ≠ Go 客户端登录密码，需两侧分别重置
3. **设备 owner 绑定**：客户端必须在 RustDesk 客户端登录（API 服务器 21114）产生活跃 client session，`ApplyActiveSessionOwner` 才会把 `peers."user"` 绑定为登录账号；无登录的连接 user 为空
4. **面板 settings 读取 auth.db**（`db.getSetting` 经 `openAuth()`），不是 `db_v2.sqlite3`
5. **浏览器缓存 `?v=` JS**：面板静态 JS 带 `?v=<version>.<timestamp>` 版本参数（页面渲染时生成），改 JS 后需重启面板使时间戳变化，否则浏览器用旧缓存
6. **GitHub push**：本机代理失效，必须 `env -u http_proxy ... git -c http.proxy= -c https.proxy= push` + `https://<token>:<token>@github.com/...` 格式
7. 本机 `21117` 被用户自启 `bdserver.exe` 占用——本机测试 Relay 会端口冲突（环境问题，非代码 bug）
8. **连接模式（P2P/仅中继）面板保存禁用**：`serverConnectionConfigService.js` 曾硬编码 `betterdesk-server.service`，fork 部署为 `betterdesk-master.service` → 检测不到 → `writable=false`。已改为候选服务名探测（`resolveSystemdUnit()`）；保存写入 systemd 单元 Environment（`P2P_FIRST`/`ALWAYS_USE_RELAY`/`P2P_FALLBACK_MS`/`SAME_NAT_RELAY`），需 `daemon-reload`+重启 Go 服务生效（面板"保存并重启"按钮处理）
