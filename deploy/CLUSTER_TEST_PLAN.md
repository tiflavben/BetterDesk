# BetterDesk 多中继集群 — 真机测试计划（3 节点）

## 拓扑

```
                    ┌─────────────────────────────┐
                    │  主控 MASTER (control plane) │
                    │  -mode signal + API :21114   │
                    │  PostgreSQL :5432 (共享票据)  │
                    │  RELAY_SERVERS=relay1,relay2  │
                    └──────────────┬──────────────┘
                                   │ 21116/21115/21118
                    ┌──────────────┼──────────────┐
                    ▼              ▼              ▼
         ┌───────────────┐  ┌───────────────┐  RustDesk 客户端 ×2
         │ RELAY 1       │  │ RELAY 2       │  (A→B 会话)
         │ -mode relay   │  │ -mode relay   │
         │ :21117        │  │ :21117        │
         │ PG→master     │  │ PG→master     │
         └───────────────┘  └───────────────┘
```

- 集群共享同一 `id_ed25519`（主控生成，复制到 relay）
- 两个 relay 节点的 `RELAY_TICKET_STORE=db`，DSN 都指向主控 PostgreSQL
- signal 侧 `RELAY_SERVERS=relay1_ip:21117,relay2_ip:21117`（顺序固定 = 哈希索引）
- 主控只跑 signal+API（**不**跑 relay）；relay 节点只跑 relay

## 关键参数

| 参数 | 主控 | relay1 | relay2 |
|---|---|---|---|
| 模式 | `-mode signal` | `-mode relay` | `-mode relay` |
| 票据存储 | `RELAY_TICKET_STORE=db` | `RELAY_TICKET_STORE=db` | 同左 |
| DB | `postgres://betterdesk:***@127.0.0.1:5432/betterdesk` | `postgres://betterdesk:***@MASTER_IP:5432/betterdesk` | 同左 |
| 监听 | 21116/21115/21118/21114 | 21117 | 21117 |

## 测试用例（按序执行）

### TC-1 PG 建表 + 探测修复验证（核心）
- 操作：主控启动后 `psql -d betterdesk -c '\dt relay_*'`
- 预期：`relay_tickets` / `relay_ticket_used` 存在
- **关键**：修复前 `r.pg` 恒 false，PG 上所有 `?` 语句语法错误 → INSERT/UPDATE 全失败（表能建但操作全拒）。修复后 Authorize/Claim 正常。**任何一次成功的中继会话即证明 PG 路径生效**

### TC-2 relay 日志确认共享存储
- 操作：`journalctl -u betterdesk-relay1 -n 50 | grep ticket`
- 预期：`[relay] Using DB-backed ticket store`（显式 db，无 WARN 强制切换）

### TC-3 跨机票据认领（修复前必然失败的核心场景）
- 操作：两台 RustDesk 客户端（A、B）配置服务器=主控 IP → A 连 B，**关闭 P2P 打洞或 NAT 后打洞失败**（强制走 relay）
- 预期：会话建立（relay 兜底）；PG 侧观察到 ticket 生命周期：
  ```sql
  SELECT uuid, initiator_id, target_id, claims, expires_at FROM relay_tickets ORDER BY rowid DESC LIMIT 5;
  SELECT COUNT(*) FROM relay_ticket_used;
  ```
  claims 从 0→2（两次 claim：A、B 各一），随后行被 DELETE + tombstone 写入 `relay_ticket_used`
- 证据：relay1/relay2 日志出现 claim/配对成功；`relay_ticket_used` 计数增长

### TC-4 哈希分配（多中继负载分布）
- 操作：建立 **4-6 个不同客户端对**的会话（不同 peer ID 组合）
- 预期：会话分布在两个 relay 上（查 `relay_tickets` 或 relay 日志 UUID claim 归属统计），而非全部命中 relay1
- 证据：relay1/relay2 各自的配对成功日志计数 > 0

### TC-5 relay 故障切换
- 操作：`systemctl stop betterdesk-relay1` → 新建 2 个会话
- 预期：新会话全部成功（signal 哈希 modulo 重映射，全部命中 relay2——已知"全量重映射"限制，正好实测）；进行中的旧会话按设计断开
- 恢复：`systemctl start betterdesk-relay1` → 新建会话重新分布

### TC-6 并发压力
- 操作：同时发起 5-10 个 A→B 会话
- 预期：全部成功；PG 无锁等待错误；relay 日志无 `Unauthorized relay UUID`（修复前症状）

### TC-7 重放防护（跨机）
- 操作：抓取一个已消费 UUID（tombstone 在 `relay_ticket_used`），手动尝试重放（或依赖单测已覆盖）
- 预期：`relay_ticket_used` 中的 UUID 在过期前不可重新 Authorize（真机抽查可跳过，单测已覆盖）

## 客户端配置（RustDesk）

```
ID/服务器 → 服务器: 主控IP
Key: 主控生成的 id_ed25519.pub 内容
```
强制 relay 场景：客户端设置里禁用"直接 IP 访问"/P2P，或两客户端在不同 NAT 后（打洞失败自动兜底 relay）。

## 已知限制（测试时留意，非缺陷）

1. 一致性哈希 = 纯 modulo：增删 relay 全量重映射（TC-5 会观察到）
2. 配对依赖进程本地 pending 表：同会话两端必须落同一 relay（哈希对称性保证）；若客户端显式 pin 了 relay 地址则跳过哈希
3. `RELAY_SERVERS` 在所有 signal 实例必须一致且同序（本测试仅 1 台 signal，无此问题）
