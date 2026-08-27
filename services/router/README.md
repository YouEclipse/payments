# Router：版本化 Context、Risk 门禁与可审计优化

Router 只回答「哪些连接能处理这次请求、哪些安全门禁允许继续、在声明的目标下如何排序、为什么」。它不持有支付凭据、不调用 PSP、不拥有 Payment/Attempt/Operation，也不决定是否可以在 `UNKNOWN` 后发起第二次资金操作。v2 保留第 4–6 章的硬资格与规则切片；v3 在独立 endpoint 上增加 Context-bound Risk、精确费用/FX、明确标为 observational 的历史表现和版本化整数评分。当前没有 ML、实时 Health、因果 uplift 或真实反欺诈能力。

## 接口与升级边界

- `/internal/v1/routing-decisions` 保留原静态请求、静态策略和返回结构；它是显式 legacy 接口。
- `/internal/v2/routing-decisions` 直接接收 `routing_context.v1`，不接受 `context` 外层包装，也不接受调用方提供的规则、能力目录、token、PAN 或 CVV。
- `/internal/v3/routing-decisions` 只接收 `routing_context.v2`。它要求完整 `risk_decision.v1`，并只使用 Router 自己加载的 Optimization artifact；调用方不能注入成本、成功率、权重、候选或 debug 字段。
- 三个决策接口都由 server 的 `X-Internal-Token` middleware 保护。非开发环境未配置 `INTERNAL_SERVICE_TOKEN` 会停止启动；开发默认值仅用于本地环境。
- v2/v3 没有各自配置时返回 `503/routing_v2_unavailable` 或 `routing_v3_unavailable`，不会改调更老 endpoint。reader-first 升级先部署目标 reader，再显式切换 Payment writer；回滚 writer 也是配置选择，不是单次失败后的自动降级。
- `ROUTER_REQUIRED_API_VERSION=v1|v2|v3` 只把 `/readyz` 绑定本部署负责的契约和 artifact。目标 reader 缺失或过期返回 503；未分配的可选 reader 过期不会驱逐仍健康的 deployment。裸进程的兼容默认仍为 v1，任何 v2/v3 deployment 都必须显式设置；Compose 明确设为 v3。
- v2/v3 成功响应保留完整 `(candidate_id, connection_revision, provider_namespace_id, provider, merchant_account, connector, region)`。`candidate_order` 是决策证据，不是跨连接重试许可。
- `422/no_eligible_provider` 不包含 winner，`candidate_order=[]`，仍有 Context/RuleSet/Catalog 摘要、资格结果与规则 trace。Payment 不应为这个负决策创建可执行 Attempt。
- 每个 v2/v3 正面或负面决策都包含 `executable_until`：RFC3339 UTC 的排他 deadline，必须满足 `now < executable_until` 才能做首次 Provider I/O。它由 Context 在线窗口和所有实际使用的 authority 取最早值，不是 retry lease；已 dispatch operation 的查询/同键恢复仍固定原 identity，不能重新路由。

请求和响应的正式契约见 `contracts/internal/router-api.openapi.yaml`。固定 Context fixtures 是 `contracts/internal/fixtures/routing-context-v1.json` 与 `routing-context-v2.json`；它们用于离线 wire/digest 回归，不是可以永久用于线上请求的样本。

## Context 与 Risk 的事实边界

`base` 包含 Payment/Attempt/Merchant ID、最小货币单位整数金额、currency、payment_method、operation、capture_method。`merchant` 包含明确配置的 entity 与已知商户国家。`payment_method_context` 包含 category，以及带来源和有效期的 network、issuer_country、credential_domain。

Observed 的 wire 顺序是 `state,value,source,source_version,observed_at,expires_at`。`known/stale` 必须有 value 和 expiry；`unknown/not_applicable/redacted` 禁止 value，不能用空值表达已知。过期、未知或不适用的元数据不参与规则事实匹配。国家约束明确指定 `merchant.country` 或 `payment_method_context.issuer_country`，不在二者间自动猜测。

所有时间必须是 canonical UTC RFC3339Nano（`Z`，无无意义末尾小数零）。新请求的 `built_at` 不得在未来或早于当前时间五分钟，known 事实必须在接收和评估结束时都有效；部署时需要同步服务时钟。纯决策函数始终只使用 `built_at`，不读系统时钟。摘要是按固定 struct 字段顺序紧凑 JSON 编码的完整 SHA-256，带 `sha256:` 前缀。

当前 Payment 只产生 `sale/automatic`，v3 Optimization 也明确只接受这个 slice。v2 Context 类型仍能表示首次 `authorize/manual`，但这不等于 Payment 已实现授权生命周期；capture/void/refund 不允许重新进入首次路由。

`routing_context.v1` 明确禁止 `risk`；`routing_context.v2` 明确要求 `risk_decision.v1`。Risk Decision 的 `subject_digest` 绑定 base、merchant、payment method metadata 及其 provenance，避免只绑定金额或一个外部 ID；完整 Context digest 还绑定 built_at 与 Risk Decision 本身。Router 在 built_at 验证 decision ID、action、版本、来源、原因、有序性和排他 expires_at，再生成只含 enforcement 结果的 Risk evidence。Router 不接收原始欺诈特征、设备指纹或凭证。

动作语义固定：`allow` 才可继续；`block`、`manual_review` 都无 winner；当前 SALE 切片没有 3DS 执行能力，因此 `require_3ds` 明确转成 `authentication_unsupported`，不能偷偷当 allow。Risk 过期会缩短 `executable_until`。这个门禁只授权首次 dispatch；一笔已发出的 operation 后续按冻结 identity 恢复，不重新跑今天的 Risk，也不因 Risk 过期改选通道。

## Router-owned 配置

`ROUTER_V2_CONFIG_JSON` 的顶层只有两个必填对象：`rule_set` 和 `capability_catalog`。`ROUTER_V3_CONFIG_JSON` 在同一 config 中额外强制 `optimization`；把 Optimization 放进 v2 或从显式 v3 配置中省略都会阻止启动。读取后一次性校验、复制、规范化并编译为不可变快照；这轮不实现热发布和控制面。非法、半份、manifest 漂移或互相矛盾的配置直接阻止启动。

非开发环境没有显式 config 时不会安装 v2/v3 fixture：相应 endpoint 返回 503；Payment 自身的未设置版本默认仍是 v2。原 `ROUTER_CONFIG_JSON` 的生产配置要求仍然有效。只有 local/dev/development/test 可以使用 `cmd/server/v2_config.go` 与 `v3_config.go` 中明确的本地模拟器 fixture；本地 Payment 默认 v3，Risk 默认 `local_fixture`，而该 Risk 模式在非本地会硬拒绝启动：

| Currency | Connection | Namespace |
| --- | --- | --- |
| USD、BRL | simulator_a_us_primary / simulator_a_us_primary_v1 | simulator_a |
| EUR | simulator_a_eu_backup / simulator_a_eu_backup_v1 | simulator_a |
| GBP 及其他未声明值 | 无候选 | 无 |

本地 descriptor 使用商户国家 US 与显式 `simulator_a` credential domain，包含固定的长有效期。这是可重复的 simulator 数据，不是生产 freshness 默认值。v3 的费用、FX、Performance 与 Scoring profile 则从进程启动时间生成短有效期 fixture，用来演示 authority 收缩；它们不声称代表任何 PSP 商业合同或未来成功率。

### RuleSet

RuleSet 必须包含 `schema_version=routing_rules.v1`、version、published_at、effective_from、expires_at、覆盖整个 catalog manifest 的 `default_candidate_order` 和 `rules` 数组（无规则也要显式 `[]`）。每条规则必须有 rule_id、1–10000 的 priority、condition、action、reason_code。优先级数值越大越高。

受限条件 AST 只有四种形式，禁止代码、正则、浮点、remote lookup 或任意字段访问：

```json
{"op":"eq","field":"base.currency","value":"USD"}
{"op":"in","field":"merchant.country","values":["US","GB"]}
{"op":"range","field":"base.amount_minor","min":1,"max":100000}
{"op":"all","all":[{"op":"eq","field":"base.currency","value":"USD"}]}
```

`eq/in` 只接受已注册的字符串字段，字面量必须符合相同字段的格式或枚举；`range` 只接受 `base.amount_minor` 的闭合正 int64 区间；空 `all` 明确表示无条件。三值 AND 中 false 优先于 unknown；未知条件记录 `truth=unknown`，不当作命中。最多 256 条规则，每条 AST 最多 64 节点、深度 8；未注册字段、同字段约束交集为空、缺失目标和同优先级可重叠的互斥 force/排序动作都会在编译时拒绝。

Action 形式如下：

```json
{"type":"prefer","candidate_ids":["conn_us","conn_eu"]}
{"type":"exclude","candidate_ids":["conn_us"]}
{"type":"force","candidate_ids":["conn_us"]}
{"type":"split","weights":[{"candidate_id":"conn_us","weight":7500},{"candidate_id":"conn_eu","weight":2500}]}
```

执行语义固定为：所有命中 exclude 都是硬约束；最高优先级命中 force 再缩窄到唯一目标；最高优先级命中 prefer/split 决定剩余集合的顺序。更低优先级的排序动作不能覆盖已选中的高优先级排序动作。没有排序规则时使用 default order。

`prefer` 只能把仍 eligible 的目标提前。`force` 的目标不 eligible 或被 exclude 时，结果为空，不会放行其他连接。`split` 的正整数权重总和必须为 10000；先移除不 eligible 的成员，再按余下权重重新归一，空集合直接拒绝。固定 SHA-256 分桶键是 `(rule_set.version, rule_id, payment_id)`，不使用 attempt_id、随机数或文件顺序；只有相同输入、相同有效候选集合和相同 artifact 才承诺相同结果。这是确定性配置分流，不是实验框架。这里描述首次路由，已有 Attempt 的执行连接仍由 Payment/Gateway 固定。

规则列表按 rule_id、集合操作数按值、AND 子节点按 canonical JSON 规范化；默认顺序和 prefer 的顺序保留。相同优先级冲突不能靠文件先后或 rule_id 悄悄解决。RuleSet digest 绑定完整规范化 artifact，`policy_version` 始终等于 `rule_set_version`。

### CapabilityCatalog

Catalog 包含 `schema_version=capability_catalog.v1`、version、published_at、effective_from、expires_at、完整 exact candidate manifest，以及每个 candidate 恰好一份 descriptor。重复 ID、物理执行 tuple 的别名、同 namespace 对应不同 PSP、缺失 revision/namespace、半份 descriptor 都会被拒绝。

Descriptor 包含 candidate_id、`enabled=yes|no|unknown`、country_field，以及 currencies、countries、payment_methods、operations、capture_methods、credential_domains 六个集合；每个集合必须显式声明 `state=known|unknown`。known 空集合表示明确不支持任何值，unknown 不带 values；两者都不是 ANY。descriptor 还必须有 source、source_version、observed_at、effective_from、expires_at。

每个候选返回 `eligible/ineligible/unknown` 及全部已检查的稳定原因码。任意明确 no 得到 ineligible，缺失当前事实得到 unknown，只有全部已知且支持的约束才是 eligible。过期 descriptor 只表示过去的能力，不能声称当前 yes 或 no。凭据域不匹配是 `token_not_portable`；域未知/过期是 `token_binding_unknown`；两者都关闭资格。

新请求不会让规则看见不合格连接。HTTP 返回 winner 前再次读取时钟，确认 Context、RuleSet、Catalog 以及所有候选 descriptor 仍然有效；过期 Router 快照返回 `503/routing_snapshot_unavailable`。

## v3 Optimization 的固定顺序

v3 复用完全相同的 Context admission、Capability 与 RuleSet。之后的顺序不可配置成“先算分再过滤”：

```text
routing_context.v2 strict admission
→ Capability eligible / ineligible / unknown
→ deterministic hard RuleSet
→ Risk allow / block / require_3ds / manual_review
→ exact Cost + observational Performance evidence
→ versioned whole-batch Scoring policy
→ exact selected connection + executable_until
```

Risk 非 allow 时不产生 Optimization；Risk allow 但硬约束已经清空候选时也不伪造空评分。只有仍允许的 exact `(candidate_id, connection_revision)` 进入 feature stages，且 Cost/Performance 的完整 manifest 必须与 CapabilityCatalog 一致。配置不能借同名候选移动 revision，结果中的每一行也继续绑定这一 identity。

### Cost 与跨境分类

FeeCatalog 的合同场景分为 attempt、success-only、failure-only。`succeeded_cost_micros` 与 `failed_cost_micros` 各自包含 attempt cost，两者不会互相相加；所有金额是报价币种 major unit 的百万分之一，输入 Payment 金额仍是原币种 minor unit 整数。币种 exponent 由版本化 metadata 提供；百分比、FX 方向与 rounding 都显式记录。Router 不猜反向汇率或 1:1，缺 schedule/FX、过期证据和算术异常都以明确状态或失败表达。

跨境费用不由 `merchant_country != provider_region` 这种隐式猜测产生。每个需要跨境判断的 exact revision 使用命名的 `merchant.country` 或 `payment_method_context.issuer_country`、对手方国家、rule version、来源和有效期；unknown/stale 从不当作 domestic。完整 classification 与 country provenance 进入响应，相关 rule/country/FX/schedule 到期都会缩短 `executable_until`。

### Performance 的统计含义

PerformanceSnapshot 只描述版本化 cohort 下、历史路由策略条件内的已终结逻辑 operation。当前 v3 只接受首次 card SALE/automatic cohort，UNKNOWN 不当作已终结失败，重复 transport delivery 不重复计数。固定 L0→L1→L2→L3 hierarchy 从精确 country/network/currency 逐步退到 candidate-wide bucket；稀疏、结算不完整或过期 bucket 不能因为观测率高就被挑中。

返回的 point estimate 使用 parent shrinkage；`observed_lower_95/upper_95` 是原始二项观测的 Wilson interval，不是 shrinkage posterior，也不是因果置信区间。`interpretation=historical_policy_conditional_not_causal` 是强制 wire 字段。全局 prior 可作为显式 `prior` evidence，但 profile 是否允许这种 unavailable feature 继续由下一阶段整体决定。

### Scoring 与 abstention

ScoringProfile 固定 quote currency、成功/成本权重、成本归一化区间、rounding version 与排他有效期。`full_v1` 同时要求 Cost 和 Performance，即使某个权重为零也不跳过证据；它对 conservative success PPM 与 succeeded-cost scenario 分别归一化后用整数 basis points 合成 0–10000 points。该分数不是 expected utility、cost per success 或因果收益。

feature 不可用时只允许 profile 声明的 whole-batch 行为：`static_order` 对整批候选回到冻结 Rule rank，或 `reject` 清空候选。不能只给缺数据的某一行一个方便的默认分。相同总分按 Rule rank、candidate ID 稳定打破；响应保留每行 Cost、Performance、numbers、profile/config version 与 digest，Payment 校验严格 allowlist、字段存在性、状态组合、identity 和 top-level order，但不在编排层复制 Router 的数学模型。

## 首次执行与恢复边界

`decision_id` 是完整 versioned decision envelope（不含 HTTP error 字段）的内容摘要，`executable_until` 是其中不可篡改的必要字段。HTTP 计算结束前再次检查当前时钟；Payment 接收后也检查 deadline，并在持久化 Attempt/Operation 与第一次 Gateway 调用之间再次检查。只要到达排他边界，就 fail closed 到人工复核，不让 GC pause、队列延迟或重放旧 Context 延长首次执行权限。

Router 不参与已 dispatch operation 的恢复。Payment/Gateway 必须固定原 connection/revision/namespace/provider、operation ID、Provider idempotency key 与 payload；默认动作是 query。只有 durable never-dispatched 事实，或认证 Gateway 返回与完整 identity/request hash 绑定且尚未过期的结构化 absence proof，才能同键重提。普通 HTTP 404、retryable 标记或字符串提示不能证伪资金副作用。Risk/Router artifact 随后过期也不允许重新路由，但不会阻断沿原 operation 的安全查询。

## 验证

```sh
go test ./services/router/...
go test -race ./services/router/...
go vet ./services/router/...
```

测试覆盖跨 Payment/Router v1/v2 golden、strict JSON、三值资格、未知凭据、有效期边界、规则编译冲突、确定性 split、canonical digest、Risk action table、费用/FX 精确算术、统计 hierarchy、whole-batch scoring、并发只读、v1/v2/v3 reader-first readiness、`executable_until` 边界，以及完整负决策证据。真实首次 dispatch、结构化 absence proof、UNKNOWN 查询/同键恢复和零资金路径由 Payment/Gateway 集成测试与 Compose E2E 负责。
