# Payment 时钟安全 Runbook

## 风险与权威

Payment 的首次路由权限、recovery deadline、退避和 lease 都使用绝对时间。当前实现以 Payment 进程时钟做业务判断，以 PostgreSQL `clock_timestamp()` 做部署见证；Provider Operation 的完整身份、CAS 和同一幂等键仍是防止异键重复扣款的最后边界，但它们不能让错误的时间判断变得正确。

`PAYMENT_MAX_CLOCK_SKEW` 是进程时钟相对数据库时钟的最大允许偏差。数据库查询前后的两个进程时间与中间的数据库采样共同限定偏差；由于单向网络延迟未知，所有可能的时钟偏差都必须落在预算内。网络不确定性会消耗预算，往返时间超过两倍预算时直接 fail closed，绝不会放宽偏差。超出预算时：

- `/readyz` 返回 503；
- 新的 Confirm 在创建资金路径前返回 503，并在紧邻首次 Provider Execute 前再次见证；
- recovery worker 在每次 claim 前检查，同键重提还会在紧邻 Provider Execute 前再次见证。

已经在 Provider 边界外执行的 operation 不会因为时钟告警改换连接、revision、namespace、operation ID、幂等键或 payload。

## 部署门禁

1. 对 Router、Payment、Gateway、PostgreSQL 与宿主节点启用同一受控 NTP/时间服务，并监控 offset、同步状态和 leap 事件。
2. 在目标网络测量 Payment 到 PostgreSQL 的高分位往返时间与正常 offset。`PAYMENT_MAX_CLOCK_SKEW` 必须显式设置，既覆盖正常抖动，又显著小于最短路由 authority headroom 和 recovery lease 安全裕量；不能照抄本地 `500ms`。
3. 发布前主动制造超过预算的时钟见证结果，确认 readiness 变红、Confirm 不到 Router/Gateway、recovery budget 不减少；恢复校时后再确认自动回绿。
4. 告警必须区分数据库不可用与 clock unsafe，并关联 NTP offset、数据库时间、节点、实例与部署版本。

## 事故处理

1. 立即停止流量与 recovery worker 扩容，不要通过放大 skew 预算来消除告警。
2. 保留所有 `UNKNOWN` operation 的原始身份与预算；禁止切 Provider 或换幂等键。
3. 校准节点和数据库时间，确认 Router artifact 时间窗没有被错误延长。
4. readiness 连续稳定后再逐步恢复流量；已 dispatch operation 继续 query-first，同键重提仍必须满足结构化 absence proof。

## 当前边界

数据库见证只覆盖 Payment 进程。生产仍需对 Router 与 Gateway 独立监控时钟；长期演进可让 recovery claim/lease 直接以数据库时间为事务权威，并把端到端时钟偏差纳入统一 SLO。
