# Payments

`payments` 是[《Designing Modern Payment Systems》](https://github.com/YouEclipse/Designing-Modern-Payment-Systems)的配套 Go 实现。它以一个可在本地完整运行、可验证失败语义的支付系统为目标，而不是只展示互不相连的代码片段。

当前阶段建立四个独立部署单元：

- **payment**：公共支付 API，以及 Payment Intent、Payment Attempt、幂等、状态机和持久化的唯一所有者。
- **router**：根据硬约束和策略返回可解释的 provider 决策；不发起扣款，也不拥有支付状态。
- **provider-gateway**：隔离 provider 协议、认证与错误归一化；Compose 中的服务名为 `gateway`。
- **simulator**：模拟 provider 的成功、拒绝、超时及不确定结果，仅供本地开发和自动化测试。

> [!CAUTION]
> `simulator` 绝不能部署到 live/production 环境，不能连接真实商户凭证，也不能被配置成真实资金通道。本仓库中的 Compose 文件只面向本地开发。

## 工程原则

- 金额使用整数最小货币单位和明确币种，不使用浮点数。
- 客户端重试依靠持久化幂等键收敛，不能制造第二次扣款。
- provider 响应丢失时进入 `UNKNOWN`，先查询或对账，禁止盲目切换 provider。
- 授权与失败证据冲突、Provider reference 碰撞等矛盾事实进入 `review_required`，停止自动恢复并写入运营 outbox，而不是用最后到达的事件覆盖资金事实。
- Router 返回稳定 `candidate_id`、不可变 `connection_revision` 与 `provider_namespace_id`；Payment 与 Gateway 持久化相同执行身份。UNKNOWN 查询与同键重送固定原 connection/revision，reference/event 碰撞按共享 namespace 检测，recovery alias 不能绕过它。Gateway 的 revision registry 拒绝相同 ID/revision 重绑执行配置。
- PostgreSQL 是第一阶段唯一的权威状态源；不引入 Redis 或消息 broker。
- 服务之间通过稳定契约通信；payment 与 provider-gateway 只能访问各自拥有的 schema，禁止跨服务读表。
- 系统只处理 tokenized payment method；日志、事件和指标不得包含 PAN、CVV 或密钥。

## 本地启动

需要 Go 1.25、Docker 和 Docker Compose v2。

```bash
cp .env.example .env
make compose-up
```

默认端口：

| 组件 | 地址 |
| --- | --- |
| payment | `http://localhost:8080` |
| router | `http://localhost:8081` |
| provider-gateway | `http://localhost:8082` |
| simulator | `http://localhost:8083` |
| PostgreSQL | `localhost:5432` |

每个 HTTP 服务提供 `/livez` 和 `/readyz`；Compose 使用 readiness 检查决定依赖服务何时可以接收请求。

本地 Gateway 仍注册旧的 `simulator_a` connection，专门恢复升级前已经持久化的 operation、outbox 与 webhook；Router 不会选择该 alias，Gateway 也拒绝用它创建新 operation。它与 canonical connection 共享 `simulator_a` namespace，但只有显式 `PROVIDER_RECOVERY_ALIASES` 才授权 webhook 跨路径关联。保留原连接、revision 和认证材料，直到非终态 operation、待投递事件及约定的迟到事件窗口排空；Gateway readiness 会在非终态 tuple 无法解析或同 revision 配置重绑时失败。完整约束见 [ADR 0002](docs/adr/0002-immutable-provider-connections.md)。

`payment` 的 `003` 与 `provider-gateway` 的 `0002` 是一次协调升级，不支持 rolling 或 zero-downtime 部署。执行迁移前必须停止接收新的 Confirm，先让所有非终态 operation 完成查询、对账或人工接管，再停止全部旧版 Payment/Gateway 进程并备份数据库；只有这些门禁都满足后，才能执行迁移并启动新版本。无法满足时不得提前应用这两份迁移，否则旧 binary 会继续写入 v1 结构并失败。

需要停止本地服务时执行：

```bash
make compose-down
```

数据库 volume 默认保留，方便连续开发。若确实需要清除本地数据，应显式运行 `docker compose -f deploy/compose/compose.yaml down --volumes`，该操作不可恢复本地数据库内容。

### 跑通第一笔支付

创建一个仅接受 token 的 Payment Intent：

```bash
curl --request POST http://localhost:8080/v1/payment_intents \
  --header 'Authorization: Bearer local-api-key' \
  --header 'Idempotency-Key: create-order-1001' \
  --header 'Content-Type: application/json' \
  --data '{
    "merchant_reference": "order-1001",
    "amount_minor": 5200,
    "currency": "USD",
    "payment_method_token": "pm_sim_approved",
    "capture_method": "automatic"
  }'
```

复制响应中的 `id`，再确认该 Payment Intent：

```bash
curl --request POST http://localhost:8080/v1/payment_intents/<PAYMENT_INTENT_ID>/confirm \
  --header 'Authorization: Bearer local-api-key' \
  --header 'Idempotency-Key: confirm-order-1001'
```

`pm_sim_approved` 会同步成功，`pm_sim_declined` 会得到明确拒绝，`pm_sim_timeout_then_succeeded` 会先返回 `processing`，随后由原 operation 的查询或 webhook 收敛为 `succeeded`。最后一种场景不会创建新的 Payment Attempt，也不会切到另一家 provider。

## 常用命令

```bash
make help
make format
make test
make test-race
make test-integration
make test-e2e
make contracts-check
make build
make docker-build
make ci
```

## 文档

- [系统架构](docs/architecture.md)
- [ADR 0001：服务边界](docs/adr/0001-service-boundaries.md)
- API 契约位于 `contracts/`。

## 关键语义依据

- [Adyen API idempotency](https://docs.adyen.com/development-resources/api-idempotency/)：同一幂等键重试、账号作用域与跨 Region 边界。
- [Stripe idempotent requests](https://docs.stripe.com/api/idempotent_requests)：同一键的参数一致性与结果重放语义。
- [PayPal idempotency](https://developer.paypal.com/reference/guidelines/idempotency/)：请求超时后沿用同一 request ID。
- [Adyen webhook handling](https://docs.adyen.com/development-resources/webhooks/handle-webhook-events/)：验签、持久化、快速确认以及重复/乱序处理。
- [Stripe separate authorization and capture](https://docs.stripe.com/payments/place-a-hold-on-a-payment-method)：Authorization hold 与 Capture 的资金语义区别。

## 当前实现边界

当前纵向切片只支持一次性 tokenized card 与 `capture_method=automatic`，其初始资金操作是单个 `SALE`。手动 Capture、Void、Refund、3DS、真实 PSP connector、账务台账与清结算会随本书后续章节逐步加入。

CI 会用真实 PostgreSQL 启动四个容器，覆盖同步成功、明确拒绝、响应丢失后安全收敛、商户幂等，以及 UNKNOWN 场景只有一条 Attempt/Provider Operation 资金路径。Router 的本地配置包含同一 simulator provider 的两个独立 connection，用于证明恢复不会漂移到备用账户或区域；外部 provider 仍然只是 simulator。

这个仓库是教学实现，也会尽量保留生产支付系统真正重要的语义。它不是经过 PCI DSS 认证的即用型支付产品；接入真实资金前仍需完成安全评审、合规认证、密钥管理、灾备和运营控制。
