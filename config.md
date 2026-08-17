# BetterDesk 项目状态文档（config.md）

> 本文档供后续开发续接使用。**禁止写入任何凭据**（API key、数据库密码、PAT、SSH 私钥、管理员密码、会话 cookie）——一律以 `[REDACTED]` 表示。
> 最后更新：2026-08-17（第五轮 P1/P3 遗留处理：?v= 内容 hash、字体本地化、gzip 单测、坑 23-25）

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
| 101 | 主控（Signal + API + 面板 + PostgreSQL） | 192.168.1.101 | `betterdesk-master.service`、`betterdesk-console.service` | 21114 (API/Signal)、21115 (Signal TCP)、21116–21119、21121 (客户端 API, TLS)、21122、5000 (面板, HTTP→307)、5432 (PG)、5443 (面板 HTTPS) |
| 102 | Relay 1（relay-only） | 192.168.1.102 | `betterdesk-relay1.service` | 21117 |
| 103 | Relay 2（relay-only） | 192.168.1.103 | `betterdesk-relay2.service` | 21117 |

- 面板 URL：`https://192.168.1.101:5443`（自签名证书，首次访问需手动接受；管理员 `admin`，密码 `[REDACTED]`；`http://192.168.1.101:5000` 自动 307 → 5443）
- 测试普通用户：`517532265` / `[REDACTED]`（viewer 角色，用于隔离验收；真实业务使用中）

### 环境变化（2026-08-10 加固后）

- **面板/客户端 API 已 HTTPS**：面板 5443、RustDesk 客户端 API 21121 均随 `httpsEnabled` 启用 TLS，自签名证书 2027-08-10 到期需轮换
- **服务运行用户**：`betterdesk`（nologin；101 uid=999，102/103 uid=988）；三台 unit 均含 `NoNewPrivileges` / `ProtectSystem=strict` / `PrivateTmp` / `ReadWritePaths`
- **防火墙**：nftables，input policy drop，放行 lo/established/22/21114–21119/21121/21122/5000/5432/5443（源限 192.168.1.0/24）
- **SSH**：纯密钥认证（`PasswordAuthentication no`）
- **PostgreSQL**：密码 24 位字母数字（明文仅存在于三台 unit 文件 DSN，`[REDACTED]`）；`archive_mode=on` + WAL 归档至 `/var/lib/postgresql/wal_archive/`
- **备份**：每日 02:00 crontab 执行 `/usr/local/bin/betterdesk-backup.sh`（`pg_dump -Fc` + 面板数据 tar，14 天保留）；回滚点 `/root/backups/20260810/`
- **21121 已 TLS 化**：RustDesk 旧客户端明文兼容需 `RUSTDESK_API_DISABLE_TOTP` 类开关或回退（见第 9 节待办）

### 关键配置（101 主控）

- `RELAY_SERVERS=192.168.1.102:21117,192.168.1.103:21117` —— **必须所有 Signal 实例完全一致且顺序一致**（Relay 哈希分配依赖顺序）
- `RELAY_TICKET_STORE=db` —— relay-only 必需，共享主控 PostgreSQL 作 ticket store
- `ENROLLMENT_MODE=open`（是否改 managed 待用户决策，见第 9 节）
- `httpsEnabled`（面板 5443 / 客户端 API 21121 的 TLS 开关，自签名证书）
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
- 最近提交链（dev，HEAD = `c7425ff`，2026-08-17 第五轮两批提交已推送 fork/dev，远端 SHA 匹配）：
  - `a88b906` fix(ui): ux35-sidebar toolkit 门控对齐 server.config、zh/zh-TW strategies 翻译、品牌字体预览本地化（移除 Google Fonts 外链，内网可用）
  - `c7425ff` fix(ui): ?v= 改内容 hash（computeStaticCacheHash，跨重启稳定）、pollConsoleRestart 改 uptime 判定防误报、新增零依赖 gzip 中间件单测 14/14（tests/gzip-middleware.test.js）
  - `80fc31e` fix(i18n): repair corrupted language files（15 文件：374 处 U+FFFD 乱码修复，对照 en.json 推断丢失字符）
  - `eee20e7` fix(ui): self-host fonts + gzip + assets（Material Icons 字体本地化消除内网 Google Fonts 21s 渲染阻塞、gzip 流式压缩+修复隐式头部 bug、logo 2.1MB→62KB、xterm 本地化+修 addon-fit 404 坏链）
  - `ca4d89b` fix(server): consolidate device auth + legacy todos（identifyDevice 8 份复制收敛公共模块、register-status 一次性领取、chat 配额文件系统持久化）
  - `5cd910d` fix(server): ws heartbeat IP binding + race fixes + authz（9 文件：WS 心跳源 IP 校验+断开摘除防 WS 劫持、peer Map 原子方法消除锁外直写竞态+race 测试、new-peer 分支 DB IP 校验、signal WS 每 IP 连接限制、CDAP/bd-mgmt 角色门禁、匿名 heartbeat 移除归属写入、relay sessionLimiter 泄漏修复）
  - `cb5a057` fix(security): device identity hardening + authz gaps（8 文件 web-nodejs：register-status 停泄露 access_token P0、identifyDevice 设备存在性校验+register/device-policy mismatch、operator/login 强制 TOTP、audit 升 requireAdmin、品牌上传扩展名白名单、dlp 按设备过滤、chat 上传配额、fleet 写端点认证、enroll token 一次性、register-request 防覆盖）
  - `de05c69` fix(server): heartbeat source IP check + db index + deploy paths（11 文件：心跳源 IP 校验防打洞流量劫持 + 新增 `TestHeartbeatRejectsSourceIPChange`、`peers."user"` 索引 `idx_peers_user` 双端、mesh 录制目录配置化 `MESH_RECORDINGS_DIR`/`RecordingDir` + 列表读取对齐、deploy 模板加 `WorkingDirectory=/opt/betterdesk`）
  - `1f88afb` fix(ui): toolkit gating + ws origin + misc（7 文件：toolkit 链接/功能块按 server.config 门控、tickets 创建角色 `isFullTicketAccessRole`、cdap-studio PUT 所有权 `isSuperAdminRole`、CDAP 契约 503 透传、`deviceStatusPush` + `serverTerminalProxy` 补 `enforceOrigin`）
  - `523462a` fix(security): close authz gaps + restore device self-service（10 文件：设备自管理恢复 viewer/operator 可管自己设备 scope 兜底、`/api/bd/attestation` + device-policy 加 `identifyDevice`、inventory/activity 拆 device/admin 路由修复 `/api/bd/device-policy` 被通配拦截、server_admin 补 `cdap.view`/`chat.access`、审计读端点加 `audit.view`、security-audit API 对齐 requireAdmin、database/stats 升 `server.config`）
  - 第一轮（更早）：`cfac560` fix(server): TOTP log leak, panic recovery, perms, db dual-backend（11 文件：TOTP 验证码仅记长度/panic recover×3/peers online-policy 权限 + org scope/LIKE 转义/org 角色/TouchAPIKey 同步/PG UpsertPeer 补 3 列/迁移对齐/时间戳格式）
  - `50dafea` fix(ui): device scope count, fleet CSRF, contract UX（29 文件：effective-scope 双解包/fleet CSRF 头/inventory NaN/负偏移/防重复提交/时区统一/i18n 24 语言补全）
  - `762f9b9` fix(security): authz hardening + XSS/upload fixes（16 文件：API key 泄露封堵/审计写入认证/设备 delete-ban 权限/系统日志-Docker server.config/票证 IDOR/CDAP 授权/策略越权/toolkit requireAdmin/布局 JSON.stringify XSS/chat 附件/SVG 上传过滤）
  - 更早：`f4d4e5c` Merge origin/dev（**上游 12 个新提交已合并**：agent Wails UI、fleet org 过滤修复、attestation 对比度、版本 bump 至 3.5.29）、`10cc31d` fix(billing): user-scoped contracts resolved & enforced、`423da9b` style(dashboard)、`ff6192e` feat(dashboard)、`0550f7f` feat(ui): 管理员/普通用户 UI 隔离、`2e2fe18` 用户资源管理、`add0c6a`/`d941ca3`/`93beda3`/`c244eff`/`1c4b80c` Relay telemetry 链
- **GitHub push 注意事项**：GitHub 直连被墙（2026-08-17 起，见坑 24），本机 git 全局配置已指向本地代理 127.0.0.1:7890（Clash），push 必须
  `env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY git push https://<token>:<token>@github.com/tiflavben/BetterDesk.git dev`（env -u 清环境变量代理后 git 自动用全局配置，勿再 `-c http.proxy=` 空覆盖；token 文件 `C:\Users\xoon\rdgen_ghbearer_token.txt`，仅本地，禁入 git/文档）
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
- 用户合同：`target_type='user'`、`target_key=<username>`；**必须引用真实存在的 `billing_packages`**（否则创建 500；UI 侧已做 package 自动解析，见第 9 节）
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
- **2026-08-10 全面扫描 + 修复 + 加固后**：`go test` 5 包全绿；安全回归 8/8 通过（HTTPS 5443 实测）；合同字段保存回归通过；relay 双节点心跳正常（CPU/RAM 有值）；加固前后对比：防火墙（nftables drop）/SSH（纯密钥）/备份（每日 02:00）/PG 密码（24 位）/服务降权（betterdesk 用户）/HTTPS（5443）✓
- **2026-08-10 第二轮（28 文件修复部署三机实测）**：device-policy 匿名 401 / 带头 200；attestation 匿名 401；security-audit 匿名 401；Go API 200；双 relay online；心跳 5s 更新；`go test` 全绿（signal 新增 `TestHeartbeatRejectsSourceIPChange` PASS）✓
- **2026-08-11 第三轮（17 文件修复部署三机实测 8/8）**：register-status 无 access_token / device-policy 匿名 401 + 伪造设备 401 Unknown device / attestation 401 / settings+audit 401 / fleet 401 / Go API 200 / 双 relay online；三机二进制 md5 一致；`go build`/`go vet`/`go test -race` 全绿（signal 40s+ 含 WS 新测试）；`node --check` 全过 ✓
- **2026-08-11 第四轮（真实浏览器回归）**：登录 21.6s→165ms、dashboard 294ms、devices 132ms（约 130 倍提速）；图标乱码消失（字体本地化）、0 外链字体请求、0 console 错误；gzip 字节级验证（`1f 8b` 魔数 + `gzip -t` VALID + `--compressed` 200）；26 个 lang 文件 JSON 全过 0 处 U+FFFD ✓
- **2026-08-17 第五轮（P1/P3 遗留处理）**：?v= 内容 hash 跨重启稳定（3.5.25.ea83ea081948 重启不变）、restart-status 返回 uptime+cacheVersion、字体本地化 googleapis=0、device-policy 401/goapi 200/login 200、gzip 单测 14/14 本地实测、console 重启 journalctl 0 错误 ✓

## 9. 进行中 / 待办

- [x] **第二轮全面扫描（6 路并行：增量审查 / Go 残余补验 / Web 二次安全 / DB 残余 / 加固后环境验证 / 协议层）**：已全部修复并部署回归（合入 523462a / 1f88afb / de05c69）
- [x] **设备页普通用户 0 台遗留（第二轮）**：确认已修复 + 实测（viewer 可管自己设备，设备自管理 scope 兜底恢复）
- [x] **/api/bd/device-policy 不可达**：inventory + activity 通配路由拆 device/admin 双 router，实测恢复可达 + 补 `identifyDevice` 认证（匿名 401）
- [x] **设备页普通用户显示 0 台**：已修复 + 回归（viewer 可见自己设备 1300228927，操作他人设备 486608902 返回 403）
- [x] **全面 bug 扫描**：5 路并行（安全 12 / 功能 10 / Go 9 / DB 7 / 运维 11），已全部修复（合入 762f9b9 / 50dafea / cfac560）
- [x] **浏览器级 UI 双视角联调**：HTTPS 5443 实测——安全回归 8/8 通过，viewer 越权访问全 403/401/404；管理员走查通过
- [x] **用户合同创建 package 自动解析**：已修复并回归（不再因缺 package 报 500）
- [x] **合同字段保存 Request failed**：PATCH→PUT + panel 前缀修复；浏览器改 4→改 3 回归通过，Go API 双源确认
- [x] **上游合并后复验**：`go test` 5 包全绿、relay 心跳/流量计量链路正常（见第 8 节）
- [x] **gitignore 策略**：`bin/`、`github_pat.txt` 已忽略
- [x] **第三轮多路扫描 + 交叉验证**（4 路独立：web 双视角 + Go 双视角，两路报告交叉比对，独报项独立复核）
- [x] **register-status access_token 泄露（P0）**：已修复 + 实测无泄露
- [x] **identifyDevice 设备伪造伞**：X-Device-Id 仅标识 → 加存在性校验，实测伪造 401 Unknown device
- [x] **operator/login TOTP 绕过**：已修复（403 totp_required）
- [x] **WS 心跳劫持**：UDP 校验补齐 WS 面 + 断开摘除 + 连接限制
- [x] **第四轮乱码修复**：图标字体加载失败 + lang 文件 374 处 U+FFFD（80fc31e / eee20e7）
- [x] **第四轮打开缓慢**：Google Fonts 渲染阻塞 21.6s → 字体本地化 + gzip + logo 缩小（约 130 倍提速）
- [x] **新增**：identifyDevice 8 份复制实现未收敛为公共模块（后续可重构）→ 已收敛公共模块（ca4d89b）
- [x] **新增**：register-status 一次性领取需 dbAdapter 置空方法（TODO 已注释）→ 已实现
- [x] **新增**：chat 上传配额为 in-memory（重启清零，后续可持久化）→ 已文件系统持久化
- [x] **新增**：JS 拆包评估（结论：不拆——gzip 后仅 42KB、?v= 全局 hash 下拆包缓存收益为零、169 函数闭包耦合；收益≈0 风险实存）
- [x] **新增**：?v= 缓存失效改内容 hash（computeStaticCacheHash，二进制扩展名正确排除；附带收益：集群多节点 cacheVersion 一致）
- [x] **新增**：品牌字体预览外链（已本地化 /fonts/<safe>/font.css + onerror 降级；字体列表本身是硬编码 CURATED_FONTS 不依赖外网）
- [x] **新增**：devices 页 strategies i18n（zh/zh-TW 已翻译；其余 22 语言仍英文原文=观察项）
- [x] **新增**：gzip 中间件专项单测（零依赖 tests/gzip-middleware.test.js 14/14，含 drift 检查）
- [x] **新增**：Web Remote WS 误伤审查（结论：无误伤——Web Remote 走 TCP 21116 不经 WS 心跳；rdclient 从不发 RegisterPeer）
- [x] **新增**：pollConsoleRestart 适配内容 hash（uptime 判定：uptimeDropped/freshProcess/cacheVersionChanged 三条件 OR）
- [x] **新增**：ux35-sidebar Toolkit 门控不一致（已对齐 server.config，与 classic sidebar 一致）
- [ ] 远程桌面 relay 数据通路：面板 `/ws/relay` WS 代理需指向真实 relay（102/103）——本轮未处理，仍待验证
- [ ] 测试用户 517532265 及其合同清理与否待用户确认（真实业务使用中）
- [ ] LAN 直连流量不经过 relay（RustDesk 架构），不计入合同流量——如需计费需另行设计（架构边界）
- [ ] **新增**：面板自签名证书 2027-08-10 到期，需轮换
- [ ] **新增**：21121 TLS 化后 RustDesk 旧客户端明文兼容性实测（如需要 `RUSTDESK_API_TLS=false`）
- [ ] **新增**：`uitest_no_pkg` 合同（2/300MB/2026-12-31）为测试产物，可清理
- [ ] **新增**：ENROLLMENT_MODE=open 改 managed 待用户决策（业务在用，保持 open）
- [ ] **新增**：Web Remote 真机回归待测试机 192.168.1.14 恢复（当前 ping 100% 丢；降级代码审查已做）
- [ ] **新增**：settings.js 更新流程 101 环境浏览器冒烟（更新/重启/uptime 判定实测）
- [ ] **新增**：其余 22 语言 strategies_title 仍英文（观察项）
- [ ] **新增**：CSP styleSrc 仍含 googleapis 白名单（middleware/security.js，未清理）
- [ ] **新增**：betterdesk-agent-client Tauri 壳仍引用 Google Fonts（index.html/tauri.conf.json）

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
curl -s -H "X-API-Key: *** root@192.168.1.101 'cat /etc/betterdesk/.api_key')" http://192.168.1.101:21114/api/users
```

> 注：部署后需 `chown betterdesk:betterdesk` 新文件（服务降权运行），详见第 11 节坑 10。

## 11. 常见坑

1. **EJS 模板风格**：`dashboard.ejs`/`settings.ejs`/`devices.ejs` 是 `<%- include()` 模板**字符串**结构——内部用 `${...}` 插值，**不能插入 `<% %>` EJS 标签**（编译报 "matching close tag"）；条件渲染用模板字符串内三元 `${cond ? `...` : ''}`
2. **两套用户库密码不同步**：面板重置密码 ≠ Go 客户端登录密码，需两侧分别重置
3. **设备 owner 绑定**：客户端必须在 RustDesk 客户端登录（API 服务器 21114）产生活跃 client session，`ApplyActiveSessionOwner` 才会把 `peers."user"` 绑定为登录账号；无登录的连接 user 为空
4. **面板 settings 读取 auth.db**（`db.getSetting` 经 `openAuth()`），不是 `db_v2.sqlite3`
5. **浏览器缓存 `?v=` JS**：面板静态 JS 带 `?v=<version>.<timestamp>` 版本参数（页面渲染时生成），改 JS 后需重启面板使时间戳变化，否则浏览器用旧缓存（⚠️ 第五轮已改内容 hash——见坑 25，改 JS 无需重启面板）
6. **GitHub push**：本机代理失效，必须 `env -u http_proxy ... git -c http.proxy= -c https.proxy= push` + `https://<token>:<token>@github.com/...` 格式
7. 本机 `21117` 被用户自启 `bdserver.exe` 占用——本机测试 Relay 会端口冲突（环境问题，非代码 bug）
8. **连接模式（P2P/仅中继）面板保存禁用**：`serverConnectionConfigService.js` 曾硬编码 `betterdesk-server.service`，fork 部署为 `betterdesk-master.service` → 检测不到 → `writable=false`。已改为候选服务名探测（`resolveSystemdUnit()`）；保存写入 systemd 单元 Environment（`P2P_FIRST`/`ALWAYS_USE_RELAY`/`P2P_FALLBACK_MS`/`SAME_NAT_RELAY`），需 `daemon-reload`+重启 Go 服务生效（面板"保存并重启"按钮处理）
9. **面板已 HTTPS**：浏览器访问 `https://192.168.1.101:5443`（自签名证书需手动接受）；curl 必须加 `-sk`；HTTP 5000 自动 307 → 5443
10. **部署后必须 `chown betterdesk:betterdesk`**：服务降权运行（nologin 用户），root 属主的新文件可能读不了/写不进，导致面板或服务异常
11. **Hermes 终端脱敏陷阱**：服务器命令带 `postgres://` URL 时，Hermes 终端会把密码脱敏成 `***`——判断"密码占位符/连不上库"问题前先做字节级验证（用 Python 直读，或 `wc -c` 长度 + star_count 比对），别把脱敏当真实内容
12. **三台 unit 有 `ProtectSystem=strict`**：部署文件到 `/opt/betterdesk`、`/opt/betterdesk-console/data`、`/etc/betterdesk` 之外路径会写入失败（ReadOnlyPaths）
13. **PG 密码在三台 unit DSN（24 位字母数字）**：改密码需三台 unit 同步 + 重启（顺序：先 PG 后服务）；重启后 `journalctl` 确认无 auth failed
14. ⚠️ **web-nodejs 路由通配陷阱**：挂在 `/api/bd` 下的模块若有管理端 `GET /` 或 `GET /:id` 通配路由，会拦截 `/api/bd/device-policy` 等设备端点（剥离前缀后匹配）——inventory/activity 已拆 device/admin 双 router（`module.exports={device,admin}`，index.js 分挂载）；新增 `/api/bd` 模块前先检查有无通配路由
15. ⚠️ **拆分后端点认证暴露**：通配路由移除后，设备端点可能从"被 requireAuth 侥幸挡住"变"真匿名"——device-policy 曾因此匿名 200，已补 `identifyDevice`；拆分后必须重测匿名可达性（匿名应 401）
16. **lazyRoute 包 `module.exports` 为 `{device,admin}` 时**：index.js 需改急加载（lazyRoute 取不到 `.device`）
17. ⚠️ **Codex 夹带越权改动**：批 C 修复时 Codex 自动加了任务外的 WS limiter，子代理清理残留时又把批 D 的正当修复（P2-2 limiter）当残留误删；并发批次的"清理残留"任务与"新增功能"任务不能同时针对同一文件区，否则互相覆盖。修复 ws.go 竞态时务必先确认并行批次是否也在改同文件
18. ⚠️ **X-Device-Id 头只是设备标识不是认证**（8 处 identifyDevice 复制实现）；新增 /api/bd 端点时一律 Bearer 优先、X-Device-Id 仅注册前流程可用
19. ⚠️ **并行子代理 + Codex 修改同一文件（signal/ws.go）会互相覆盖**（批 D 复原 limiter 又被批 C 收尾删除）——Go 侧多批次并行时按包隔离而非按文件隔离
20. ⚠️ **Node 隐式头部时序**：`express.static`（send 库）setHeader + `stream.pipe(res)` 不显式调 writeHead，首次 `res.write` 先于 writeHead 触发；gzip 中间件若只在 writeHead 才初始化压缩，数据已明文写出但头声明 gzip → 浏览器解压失败页面永久 loading。修复：write 路径惰性初始化（startGzip 辅助函数，write/writeHead 双入口幂等）。验证 gzip 必须字节级（魔数 + `gzip -t` + `--compressed`），不能只看 size
21. ⚠️ **部署验证要字节级**：`curl -w size_download` 只看大小会漏检"声明 gzip 头但明文输出"类损坏（97081B 恰好看起来像压缩过）；必须验证魔数 `1f 8b` + `gzip -t`
22. ⚠️ **UI 乱码不一定是编码问题**：图标字体加载失败（ligature 变英文文本）视觉像乱码；先查 Network 面板外链字体请求
23. ⚠️ **Codex CLI 默认模型 gpt-5.6-sol 经 cc-switch 代理时 additional_tools 被丢弃**：模型称无工具、0 落地、甚至幻觉成功报告——须 `codex exec -m deepseek-v4-flash` 规避；前台管道+pty 易 420s 挂起，改后台+日志文件
24. ⚠️ **GitHub 直连被墙（2026-08-17 起）**：须走全局 git 配置本地代理 127.0.0.1:7890（Clash）；env -u 清环境变量代理后 git 自动用全局配置，勿 `-c http.proxy=` 空覆盖
25. ⚠️ **内容 hash 替代 Date.now() 后**：依赖"cacheVersion 变化"判定的前端逻辑（如更新轮询）会失效——改判 uptime（新进程从 0 起）
