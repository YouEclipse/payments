# ADR 0003：冻结 Context、Risk 与 Router v2/v3 reader-first 契约

状态：已实现的本地/测试纵向切片。真实 PSP metadata resolver、授权 Risk adapter、配置审批控制面、因果优化与独立 Decision Store 不在本 ADR 内。

## 背景

Payment 需要在不把 token、PAN、CVV、商户密钥或调用方策略交给 Router 的前提下，得到可解释且可持久化的首次路由决策。仅返回一个 PSP 名称无法回答：哪些硬资格被检查、Risk 是否允许继续、成本和历史表现来自哪个版本、第一次 Provider I/O 还能否安全发生，以及日后为什么选择了这条 exact connection。

另一方面，增加 v3 不能破坏 v1/v2 reader。协议升级必须是显式 writer 选择，而不是某次新接口失败后自动回退；旧 Context、过期 Risk 或过期 feature artifact 也不能通过排队或重放延长资金操作权限。

## 决策

Payment 拥有 ContextBuilder。它读取已经持久化的 Intent、显式商户 profile 与受控 metadata source，固定无凭证 Context；商户不能通过公开 API 注入规则、候选、entity、credential domain、Risk、成本、表现或 Router artifact。Token 仅作为授权 metadata lookup 的输入，不进入 Context、摘要或 Router 请求。来源字符串提供 provenance，但不是密码学真值；信任仍依赖服务身份、源系统授权与 adapter。

Router 提供三个显式 reader：

| Endpoint | Context | 行为 |
| --- | --- | --- |
| `/internal/v1/routing-decisions` | legacy flat request | 保留静态兼容行为 |
| `/internal/v2/routing-decisions` | `routing_context.v1` | Strict admission → Capability → RuleSet |
| `/internal/v3/routing-decisions` | `routing_context.v2` | v2 hard gates → Risk → Cost/Performance → Scoring |

v2/v3 都不接受外层 `context` 包装，不接受 null、重复 key、case alias、未知字段、尾随对象或不受限输入。v3 的固定顺序为：

```text
Strict routing_context.v2 admission
→ Capability eligible / ineligible / unknown
→ deterministic hard RuleSet
→ Context-bound Risk action
→ exact Cost + observational Performance evidence
→ versioned whole-batch Scoring policy
→ exact selected connection + executable_until
```

`unknown` capability 不是 eligible。Rule force 不能复活被 Capability 排除的候选，Risk 非 allow 不能产生 winner，Scoring 也不能把任何已移除候选加回来。相同 artifact 与 Context 在 `built_at` 纯求值，不读取随机数、网络、实时 health 或隐式 wall clock；HTTP admission 与 completion 另用当前时间缩短在线 authority。

## Context、Risk 与摘要协议

- base 绑定 payment、attempt、merchant、amount、currency、operation 与 capture method。Payment 当前只发 card SALE/automatic；Router 表示 AUTHORIZE/manual 不等于 Payment 已实现完整授权生命周期。
- 观察值使用 `known`、`unknown`、`not_applicable`、`redacted`、`stale`。known/stale 需要 value 与 validity；unknown 等状态禁止 value，不能靠空字符串伪装已知。
- `routing_context.v1` 禁止 risk；`routing_context.v2` 强制 `risk_decision.v1`。Risk 的 `subject_digest` 绑定 base、merchant、payment-method metadata 及 provenance，避免只绑定金额或外部 ID；完整 Context digest 另绑定 `built_at` 与 Risk Decision。
- Risk action 只有 `allow`、`block`、`require_3ds`、`manual_review`。当前 SALE 切片无 3DS 执行能力，因此 require_3ds 明确成为 `authentication_unsupported`，不会降级成 allow。Unavailable Risk source 也从不生成 allow。
- 时间使用 canonical UTC RFC3339Nano。Risk `evaluated_at <= built_at < expires_at`；在线入口还要求 Context/Risk 在当前时钟下有效。
- `context_digest` 为 `sha256:<64 lowercase hex>`，使用两侧固定 struct 字段顺序与 Go JSON 编码。这是本协议规范，不声称通用 JSON 自然得到相同摘要，也不是 JCS。
- [v1 golden](../../contracts/internal/fixtures/routing-context-v1.json)与 [v2 golden](../../contracts/internal/fixtures/routing-context-v2.json)由 Payment/Router 两侧共同验证 wire order、omission 与 digest。

Router 把原 Risk Decision 规范化为 enforcement evidence：decision ID/digest、version、action、outcome、continue、reason codes 与 required capabilities。响应不携带原始欺诈特征、设备指纹或支付凭据。

## Optimization 证据边界

`ROUTER_V3_CONFIG_JSON` 顶层沿用 RuleSet 与 CapabilityCatalog，并强制一个 `routing_optimization.v1`。Optimization 内的 FeeCatalog、PerformanceSnapshot、ScoringProfile 与 CrossBorderRules 是 Router-owned immutable artifacts；Cost/Performance 的 complete `(candidate_id, connection_revision)` manifests 必须严格等于 CapabilityCatalog，不能引入候选或移动 revision。

Cost 使用整数与有理数完成 fee/FX 计算，显式记录 currency exponent、contract scenario、directed FX、rounding、schedule 与有效期。成功与失败费用是各自包含 attempt fee 的两种合同场景，不是 actual settlement cost 或概率加权期望。需要跨境费时，必须使用命名 country field、对手方国家、rule version 与 provenance；unknown/stale 不会猜成 domestic。

Performance 只表达声明 cohort 与历史路由策略条件下的 observational evidence。固定 hierarchy、sample/coverage gate、parent shrinkage 与 raw Wilson interval 都进入版本化契约；`historical_policy_conditional_not_causal` 是必要字段，不能把历史成功率解释成 PSP 因果 uplift。

ScoringProfile 使用独立的 conservative-success 与 succeeded-cost 目标、整数 points、显式 weights/range/rounding。`full_v1` 必须同时拥有两类 feature；缺失时只允许整个 batch 使用 `static_order` 或 `reject`，绝不逐候选填默认值。响应保留每个候选的 Cost、Performance、Classification、Score numbers、config/profile version 与 digest。Payment 严格验证字段 allowlist、状态组合、identity、order、上下文绑定和内容派生 decision ID，但不在 Orchestrator 重写 Router 的数学模型。

## 排他首次执行期限

每个 v2/v3 正面或负面决策都携带 required `executable_until`，编码为 RFC3339 UTC instant，并采用半开区间：只有 `now < executable_until` 才有首次 Provider I/O authority。它取以下实际 authority 的最早值：

- Context 的五分钟 online window 与所有 known observation expiry；
- Risk expiry（v3）；
- RuleSet、CapabilityCatalog 与正向 candidate descriptor expiry；
- ScoringProfile，以及 scored 路径实际使用的 fee schedule、FX、Performance freshness/window 与跨境 rule/country expiry。

`decision_id` 是完整 versioned decision envelope（不含 HTTP error 字段）的内容摘要，所以 deadline、Risk 与 Optimization 不能在不改变 ID 的情况下被篡改。Router 在求值结束时重新读时钟；Payment 接收时再次验证，并在数据库准备与第一次 Gateway 调用之间最后检查。到达边界后进入 `review_required`，不能用 GC pause、队列延迟或旧 Context replay 获得一次迟到 dispatch。

## 先持久化，再执行

Payment 在创建 Attempt/Operation 的同一事务中写入 `routing_context_snapshot` 与 `routing_decision_snapshot`。准备事务锁定 Intent，重新验证金额、币种、商户、Attempt、Context digest、decision ID、`executable_until` 与所选完整执行 identity。v1 snapshot 不能包含 Risk/Optimization；v2 Context 必须携带 allow Risk 与一致的 Optimization evidence。Risk block、3DS required、manual review、hard no-eligible 或 scoring reject 都不会创建可执行 Attempt/Operation。

PostgreSQL `004` 建立成对不可变快照，`006` 扩展到 Risk/Optimization 绑定。旧 Attempt 保持 NULL，不能用今天的 Context 补写历史。JSONB 会重排对象键，读回时按强类型契约重算 digest/decision ID，不能 hash JSONB 原始文本。当前持久化的是已准备执行的 Attempt 证据，不是第 14 章完整 Decision Store；被拒绝决策目前没有独立查询投影。

## Reader-first 配置与升级

| 配置 | 语义 |
| --- | --- |
| `PAYMENT_ROUTER_API_VERSION=v1|v2|v3` | Payment writer 的显式选择；无错误驱动 fallback |
| `PAYMENT_RISK_MODE=local_fixture|unavailable` | v3 Risk source；local fixture 在非 local/test 环境硬拒绝 |
| `PAYMENT_MERCHANT_PROFILES_JSON` | 严格、版本化、带时间有效性的显式商户配置 |
| `ROUTER_V2_CONFIG_JSON` | Router-owned `{rule_set,capability_catalog}`，禁止 Optimization |
| `ROUTER_V3_CONFIG_JSON` | Router-owned `{rule_set,capability_catalog,optimization}` |
| `ROUTER_REQUIRED_API_VERSION=v1|v2|v3` | 只将本 deployment readiness 绑定被分配 reader 的 authority；裸进程兼容默认 v1，v2/v3 必须显式设置 |

local/dev/development/test 未显式设置时，Payment 默认 v3、Risk 默认 `local_fixture`，Router 安装短有效期 v3 Simulator artifacts；Compose 把 required reader 设为 v3。非本地 Payment 未设置版本时仍默认 v2、Risk 默认 unavailable；Router 不安装 v2/v3 local fixture。生产 v3 需要显式授权 Risk adapter 与新鲜 v3 config，当前仓库尚未提供真实实现。

升级顺序是：先部署能够读取目标版本且 `ROUTER_REQUIRED_API_VERSION` 对应、`/readyz` 为 200 的 Router；再应用兼容迁移；最后显式切 Payment writer。目标 reader 未配置/过期返回 503，未分配 reader 的过期不会错误驱逐当前 deployment。回退 writer 也只能是操作员配置，并只影响尚未创建的 Attempt；任何 old/new endpoint 的单次 4xx/5xx 都不会触发协议降级或重路由已有 Operation。

## UNKNOWN 恢复不重新获得路由权

`executable_until` 只约束第一次 Provider I/O。一旦 operation 已 dispatch，Context/Risk/Router artifact 后续过期既不能授权新 Attempt，也不会阻止固定原 operation/connection/revision/namespace/provider/idempotency-key/payload 的查询与安全恢复。默认恢复动作是 query，绝不是重新调用 Router。

`same_key_resubmit` 只允许两类可证伪的 absence evidence：Payment durable 状态证明 operation 从未 dispatch；或认证 Gateway 返回与完整 identity、request-hash version 2 和原始 request hash 绑定、且仍在半开 validity window 内的 `gateway_recovery.v1` proof。proof 最长新鲜 5 秒，并截断本次调用时限。普通 HTTP 404、retryable hint、legacy hash、身份漂移、畸形或过期 proof 都不能证明 Provider 副作用不存在；继续查询或进入 `review_required`。当前 structured proof 只在 local/test Simulator recovery 中实现，不代表任意真实 PSP 的幂等保留期或 query 语义。

## 验证与范围

测试覆盖 v1/v2 golden、strict JSON、Capability 三值语义、规则冲突与确定性、Risk action table、费用/FX 精确算术、Performance hierarchy 与统计标签、whole-batch scoring、内容派生 decision ID、`executable_until` 边界、v1/v2/v3 reader-first readiness、SQL 快照不可篡改、首次 dispatch 过期 fail closed，以及已 dispatch operation 在 Risk 过期后的原路恢复。Compose E2E 还验证成功、拒绝、timeout→UNKNOWN 收敛、商户幂等、Risk 非 allow/no-eligible 零资金路径和一条资金 operation 不变量。

不包含真实 PSP/Risk adapter、3DS、动态配置审批、因果路由、独立 Decision Store 或 live 资金处理。当前只是可运行、可证伪的教学核心，不是经 PCI DSS 或 Provider production review 的支付产品。
