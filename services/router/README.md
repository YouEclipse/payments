# Router：版本化 Context、确定性规则与能力资格

这是第 4–6 章的有界实现。Router 只回答「哪些连接能处理这次请求、规则如何排序、为什么」。它不持有支付凭据、不调用 PSP、不决定是否可以在 `UNKNOWN` 后发起第二次资金操作，也没有 Cost、Score、ML 或实时 Health 输入。

## 接口与升级边界

- `/internal/v1/routing-decisions` 保留原静态请求、静态策略和返回结构；它是显式 legacy 接口。
- `/internal/v2/routing-decisions` 直接接收 `routing_context.v1`，不接受 `context` 外层包装，也不接受调用方提供的规则、能力目录、token、PAN 或 CVV。
- 两个接口都由 server 的 `X-Internal-Token` middleware 保护。非开发环境未配置 `INTERNAL_SERVICE_TOKEN` 会停止启动；开发默认值仅用于本地环境。
- v2 没有配置时返回 `503/routing_v2_unavailable`，不会改调 v1。reader-first 升级应先部署 v2 reader，再显式切换 Payment writer；回滚 writer 也是配置选择，不是一次请求失败后的自动降级。
- v2 成功响应保留完整 `(candidate_id, connection_revision, provider_namespace_id, provider, merchant_account, connector, region)`。`candidate_order` 是决策证据，不是跨连接重试许可。
- `422/no_eligible_provider` 不包含 winner，`candidate_order=[]`，仍有 Context/RuleSet/Catalog 摘要、资格结果与规则 trace。Payment 不应为这个负决策创建可执行 Attempt。

请求和响应的正式契约见 `contracts/internal/router-api.openapi.yaml`。固定 Context fixture 在 `contracts/internal/fixtures/routing-context-v1.json`；它用于离线 wire/digest 回归，不是可以永久用于线上请求的样本。

## Context 的事实边界

`base` 包含 Payment/Attempt/Merchant ID、最小货币单位整数金额、currency、payment_method、operation、capture_method。`merchant` 包含明确配置的 entity 与已知商户国家。`payment_method_context` 包含 category，以及带来源和有效期的 network、issuer_country、credential_domain。

Observed 的 wire 顺序是 `state,value,source,source_version,observed_at,expires_at`。`known/stale` 必须有 value 和 expiry；`unknown/not_applicable/redacted` 禁止 value，不能用空值表达已知。过期、未知或不适用的元数据不参与规则事实匹配。国家约束明确指定 `merchant.country` 或 `payment_method_context.issuer_country`，不在二者间自动猜测。

所有时间必须是 canonical UTC RFC3339Nano（`Z`，无无意义末尾小数零）。新请求的 `built_at` 不得在未来或早于当前时间五分钟，known 事实必须在接收和评估结束时都有效；部署时需要同步服务时钟。纯决策函数始终只使用 `built_at`，不读系统时钟。摘要是按固定 struct 字段顺序紧凑 JSON 编码的完整 SHA-256，带 `sha256:` 前缀。

当前 Payment 只产生 `sale/automatic`。Router 的类型系统也能表示首次 `authorize/manual`，但这不等于 Payment 已实现授权生命周期；capture/void/refund 不允许重新进入首次路由。

## Router-owned 配置

`ROUTER_V2_CONFIG_JSON` 的顶层只有两个必填对象：`rule_set` 和 `capability_catalog`。读取后一次性校验、复制、规范化并编译为不可变快照；这轮不实现热发布和控制面。非法、半份或互相矛盾的配置直接阻止启动。

非开发环境没有 v2 配置时只保留已配置的 legacy v1 能力，v2 返回 503。原 `ROUTER_CONFIG_JSON` 的生产配置要求仍然有效。只有 local/dev/development/test 可以使用 `cmd/server/v2_config.go` 中明确的本地模拟器 fixture：

| Currency | Connection | Namespace |
| --- | --- | --- |
| USD、BRL | simulator_a_us_primary / simulator_a_us_primary_v1 | simulator_a |
| EUR | simulator_a_eu_backup / simulator_a_eu_backup_v1 | simulator_a |
| GBP 及其他未声明值 | 无候选 | 无 |

本地 descriptor 使用商户国家 US 与显式 `simulator_a` credential domain，包含固定的长有效期。这是可重复的 simulator 数据，不是生产 freshness 默认值。

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

## 验证

```sh
go test ./services/router/...
go test -race ./services/router/...
go vet ./services/router/...
```

测试覆盖跨 Payment/Router golden、strict JSON、三值资格、未知凭据、有效期边界、规则编译冲突、确定性 split、canonical digest、不可变快照并发读取、v1/v2 reader-first 行为，以及完整负决策证据。真实支付执行和 UNKNOWN 恢复由独立的 Payment/Gateway 集成测试与 Compose E2E 负责。
