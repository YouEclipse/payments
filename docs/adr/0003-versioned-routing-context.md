# ADR 0003：冻结 Context、硬约束与可重放的 Router v2

状态：已实现的本地/测试纵向切片；真实 PSP metadata resolver、动态优化与独立 Decision Store 不在本 ADR 内。

## 决策

Payment 拥有 ContextBuilder。它读取已经持久化的 Intent、显式商户 profile 和受控 metadata source，固定 `routing_context.v1`；商户不能通过公开 API 注入规则、候选、entity、credential domain 或 Router 所有的 artifact。Token 仅作为 metadata lookup 的输入，不进入 Context、摘要或 Router 请求。来源字符串是可追踪信息，不是密码学证明；可信度仍依赖服务身份、源系统授权和 metadata resolver。

Router 的 `/internal/v2/routing-decisions` 只读取冻结 Context，独立加载 RuleSet 与 Capability Catalog，执行：

```text
Strict Context admission
→ capability eligible / ineligible / unknown
→ deterministic hard rule constraints
→ stable preference / split
→ selected connection + complete decision evidence
```

`unknown` capability 不可作为 eligible。Rule 的 force 不能复活被能力过滤排除的候选；exclude 全部生效，最高优先级 force 缩窄允许集合，最高优先级 prefer/split 只在剩余集合内决定顺序。冲突同优先级规则在编译时拒绝；请求期间没有代码求值、网络查询、浮点金额或当前时间隐式输入。

Split 以 rule-set version、rule ID、Payment ID 做稳定分桶，并在剩余 eligible 的权重成员中选择。它只是策略分配，不是具备 exposure/propensity/停止规则的实验，也不允许 UNKNOWN 新建 Attempt。默认顺序、规则顺序和集合输入的规范化均有确定性测试。

## Context 与 artifact 协议

- Context 的 base 绑定 payment / attempt / merchant / amount / currency / operation / capture method。Payment 当前仍只发 card SALE/automatic；Router 预留的 AUTHORIZE/manual 输入不等于公开 Payment API 已支持。
- 观察值使用 `known`、`unknown`、`not_applicable`、`redacted`、`stale`；known/stale 需要 value 与有效期。缺失字段、null、重复 JSON key、非规范字段名或未知字段拒绝。
- 时间以规范 UTC RFC3339Nano 表示。纯求值使用 `built_at`，在线入口另外校验时间新鲜度及 artifact 当前有效性，不能拿旧 Context 绕过已过期配置。
- `context_digest` 为 `sha256:<64 lowercase hex>`。两侧使用固定 struct 字段顺序与 Go JSON 编码；这是本协议的明确规范，不宣称任意 JSON 序列化都自然一致，也不是通用 JCS。
- [共享 golden](../../contracts/internal/fixtures/routing-context-v1.json)在 Payment/Router 两侧同时测试。Country/currency 只做格式检查，实际支持集合来自 Catalog；没有宣称实现完整 ISO 注册表。
- 返回 RuleSet/Catalog 的独立 version 和 digest，以及 rule_hits、eligibility、exclusions、reason_codes；HTTP 422 保留证据但没有 selected candidate。Payment 拒绝摘要漂移、policy/rule version 不一致、缺失或重复 eligibility，以及不在 eligible 集合的 winner/order。
- Router 服务外层对 v1/v2 都执行共享内部 token 鉴权；health/metrics 不使用该 token。它不是 mTLS 或细粒度服务授权的替代品。

## 先持久化，再执行

Payment 在创建 Attempt / Operation 的同一事务中写入 `routing_context_snapshot` 和 `routing_decision_snapshot`。准备事务以锁定的 Intent 重新验证金额、币种、商户、Attempt、Context digest 及所选完整执行身份。PostgreSQL `004` 增加成对快照约束与不可修改 trigger；状态归约和 UNKNOWN recovery 不写这两列。

旧 Attempt 的两列保持 NULL，不能用今天的 Context 补写历史。JSONB 会规范化对象键顺序，因此读回后按强类型 Context 重新计算摘要，不能对 JSONB 的原始文本直接做 hash 比较。新快照中没有支付 token；原 Payment/Gateway 持有 token reference 的既有安全责任仍存在。

当前没有独立 Decision Store，也没有保存所有未执行/被拒绝决策的查询投影。持久化的是已准备执行的 Attempt 证据，不应把它夸大为第 14 章完整审计平台。

## 配置和升级

| 配置 | 语义 |
| --- | --- |
| `PAYMENT_ROUTER_API_VERSION=v2` | Payment 默认使用新协议；没有错误驱动的自动降级 |
| `PAYMENT_ROUTER_API_VERSION=v1` | 显式 reader-first 兼容模式，保留旧静态请求和路由行为 |
| `PAYMENT_MERCHANT_PROFILES_JSON` | 严格、版本化、带时间有效性的显式商户配置；每个已认证商户必须有 profile |
| `ROUTER_V2_CONFIG_JSON` | Router 所有的 `{rule_set,capability_catalog}`，不来自支付请求 |

升级顺序是：先部署能读取 v2 的 Router，再应用 Payment `004` 并启用 v2 writer。`004` 为旧记录保留 NULL，不改变旧 v1 写入要求；它不能抵消 ADR 0002 所述 `003`/Gateway `0002` 的协调升级限制。回退到 v1 必须是操作员显式配置，仅影响尚未创建的 Attempt，不能重路由已存在的 Operation。

只有 local/dev/development/test 的单个 `merchant_demo` 获得内建 profile（启动后 24 小时过期）；其他商户和所有非本地环境必须提供显式 profile。SimulatorMetadata 只识别四个固定测试 token，不凭任意字符串前缀推测通道可移植性。非本地默认 resolver 返回 unknown，无法通过凭证兼容性过滤；需要真实 vault/Provider metadata Adapter 才能接真实通道。

Router 本地 Catalog 是固定有效期的测试 fixture：USD/BRL → `simulator_a_us_primary`，EUR → `simulator_a_eu_backup`，GBP → 422。非本地没有显式 v2 配置时 v2 返回 503，不暗中启用本地 fixture；v1 reader 仍按自己的显式配置运行。

## 验证与边界

测试覆盖 Context golden、凭证最小化、未知/过期状态、规则冲突与确定性、force 不能越过 Eligibility、并发只读求值、SQL 快照绑定/不可篡改，以及 Compose 两个同 PSP connection 和 no-eligible 零资金路径。此前 UNKNOWN/幂等/namespace 冲突回归继续保留。

不包含 Cost/Success/Risk scoring、动态配置发布、健康熔断、跨 Provider Fallback、真实 PSP 或 live 资金处理。当前是可运行核心的又一个窄切片，不是完成的生产支付产品。
