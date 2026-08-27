# Payment 006 路由证据升级 Runbook

## 为什么会停止升级

Migration 004 允许 v1 `routing_decision_snapshot` 不含 `candidate_order`。Migration 006 要求所选连接必须是固定候选顺序的第一项，并增加不可续期的 `routing_executable_until`。对 004 合法但证据不完整的历史行，006 会让整个 migration 事务以 SQLSTATE `23514` 回滚；不会留下半个新列或半套触发器。

这不是可以自动 backfill 的普通字段。候选顺序是历史 Router 决策证据；仅根据最终所选连接猜一个数组会制造从未发生过的事实。

## 升级前只读预检

在生产只读副本和最新可恢复备份上都执行：

```sql
SELECT id,
       payment_intent_id,
       routing_decision_id,
       provider_connection_id,
       routing_decision_snapshot->>'context_schema_version' AS context_schema_version
FROM payment_attempts
WHERE routing_decision_snapshot IS NOT NULL
  AND CASE
      WHEN jsonb_typeof(routing_decision_snapshot->'candidate_order') = 'array'
      THEN jsonb_array_length(routing_decision_snapshot->'candidate_order') = 0
           OR routing_decision_snapshot->'candidate_order'->>0 IS DISTINCT FROM provider_connection_id
      ELSE true
  END
ORDER BY created_at, id;
```

结果必须为零行，或每一行都有可追溯的原始 Router 响应。还要在隔离恢复环境运行完整 migration 与 `go test -tags=integration ./services/payment/internal/postgres`，确认旧 operation 的 query/recovery 语义未改变。

## 备份与停写

1. 停止新的 Confirm 和 recovery worker；等待在途事务结束。
2. 停止所有旧 Payment binary，记录精确应用 SHA 与已应用 migration 版本。
3. 创建数据库一致性备份，并在隔离环境完成一次恢复验证；只有“已实际恢复并查询成功”的备份才算门禁通过。
4. 导出预检命中的行、关联 Intent/Operation、原始 Router 响应来源与审计审批记录。

## 允许的修复

只有从当时保存的原始 Router 响应恢复出精确 `candidate_order`，并验证 decision ID、所选完整 connection/revision/namespace/provider 与当前关系列一致时，才允许在维护事务中修复。操作必须：

- 锁定精确 Attempt 行；
- 临时关闭的只能是该表的 routing snapshot immutable trigger；
- 使用经过双人复核的精确原始 JSON，而不是推导值；
- 在提交前重新执行预检与身份一致性查询；
- 重新启用 trigger，并在同一事务中确认其已启用。

如果找不到原始响应，立即停止升级。不要清空快照、不要生成单元素数组、不要改写 decision ID，也不要用 migration 006 之后的新模型解释旧决策；应由数据、支付风险与合规负责人选择保留旧 reader 的兼容迁移方案。

## 执行与回滚

1. 在恢复演练环境使用候选发布 binary 自动执行 migrations，运行 PostgreSQL integration 与 Compose E2E。
2. 生产维护窗口再次执行预检；任何新命中都终止发布。
3. 启动一个 Payment 实例，确认 migration 006 已记录、`/readyz` 绿色、历史查询可读，再逐步扩容。
4. 如果 migration 失败，保持旧 binary 停止状态并保留失败日志。事务会自动回滚；确认 `006_risk_routing_evidence.sql` 未记录且新列不存在后，才可按审批恢复旧版本。

不得在 006 已成功、已有新 v2/v3 写入后直接启动不理解新约束的旧 writer。此时回退必须是 reader-compatible 的前滚修复。
