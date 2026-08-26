# ADR 0001：第一阶段服务边界

- 状态：Accepted
- 日期：2026-08-25

## 背景

本项目既是技术书的可运行配套实现，也要保留真实支付系统中的幂等、状态收敛、审计和失败处理语义。第一阶段需要展示微服务边界，但不能通过大量基础设施掩盖业务不变量。

支付系统最危险的边界错误包括：多个服务共同写同一支付状态、路由层直接扣款、provider 集成层将超时解释为失败，以及测试 provider 被误用于真实资金环境。

## 决策

系统划分为四个独立部署单元：payment、router、provider-gateway 和 simulator。它们位于一个 Go module 中，但各自拥有独立的 `main` package 和容器镜像。

### Payment 边界

Payment 提供公共支付 API，并且是 Payment Intent、Payment Attempt、API 幂等记录和 payment outbox 的唯一所有者。所有支付状态迁移和相关 durable work 必须在明确的事务边界内提交。

### Router 边界

Router 是纯决策边界。它接收 routing context，执行硬约束并返回候选顺序与 reason codes。它不调用 provider、不保存 payment 状态，也不修改 Payment Attempt。

### Provider Gateway 边界

Provider-gateway 是内部集成边界。它负责 provider-specific 协议、认证、请求映射和错误归一化，但不选择路由、不保存 Payment Intent 状态，也不能把网络错误自行解释成资金失败。它拥有自己的 operation、webhook inbox 与 outbox schema，以保证 provider 请求和回调幂等。

Router 返回的 `candidate_id` 是精确 Provider Connection 身份，包含 Provider、Merchant Account、Connector 与 Region 的绑定。Payment 与 Provider Gateway 持久化同一个 `provider_connection_id`、不可变 revision 和 provider namespace；Provider 名称只选择 adapter kind。retry/query 固定 exact connection/revision，外部 reference/event collision 按共享 namespace 检测，webhook 认证仍按连接与显式 recovery alias 授权，详见 [ADR 0002](0002-immutable-provider-connections.md)。

### Simulator 边界

Simulator 实现 provider 测试契约，用于本地开发和 CI。它必须拒绝 live/production 环境，不能读取真实 provider 凭证，也不能作为真实路由候选部署。

### 通信和数据所有权

Payment、router 和 provider-gateway 第一阶段通过同步 HTTP 通信。Payment reconciler 从自己的持久化 operation 状态恢复 UNKNOWN；状态迁移同时写 payment outbox，但第一阶段尚未连接它的 publisher。Gateway 用独立 outbox 至少一次投递归一化 outcome，Payment 以 inbox 去重。不引入 Redis 或独立消息 broker。

共享 PostgreSQL 实例不等于共享数据所有权。本地 Compose 中 payment 使用默认 namespace，provider-gateway 使用 `provider_gateway` schema，并为了启动简单共用开发账号；禁止服务跨边界直接读写表。Production 必须使用独立 role 与最小权限 grant，必要时配置独立 database 或 cluster，不能把这一边界只留在文档约定中。

## 后果

正面影响：

- 可以独立构建、部署和扩缩四个运行单元；
- 支付状态只有一个权威写入者，事务和审计边界清晰；
- router 可以在不触碰资金状态的情况下快速演进；
- provider 协议变化被隔离在 provider-gateway 内；
- 本地只依赖 PostgreSQL，故障测试易于复现；
- simulator 与 live provider 之间存在显式安全隔离。

代价：

- payment 服务第一阶段同时承载 HTTP 和 durable worker，未来可能需要拆分部署；
- 同步调用 router 会增加一次内部网络跳转，因此需要严格超时和失败策略；
- PostgreSQL outbox 的吞吐与保留策略需要监控，不能无限增长；
- 单 module 不能提供 module 级独立版本，当前以仓库 commit 和镜像 digest 作为发布身份。

## 被否决的方案

### 每个服务单独 Go module

第一阶段边界仍在形成，独立 module 会引入跨 module tag、`replace` 和 CI 矩阵管理，却不会自动增强运行时隔离。因此暂不采用；出现独立消费或 Go 版本需求后再评估。

### 首日引入 Kafka 或 NATS

当前没有测量证明需要 broker 的吞吐、多消费者或 replay 能力。数据库与 broker 双写还会新增一致性风险。第一阶段采用 transactional outbox。

### 使用 Redis 作为队列或支付状态存储

Redis 可以支持缓存和限流，但不能成为支付事实的唯一来源。第一阶段没有必要增加该运行依赖。

### Router 直接调用 provider

这会把策略决策和 provider 副作用耦合，使 UNKNOWN、重试和审计更难收敛。因此 router 只做决策；payment 控制 attempt 生命周期，并通过 provider-gateway 执行 provider 协议。

## 重新评估条件

当独立扩缩、明确的团队所有权、外部 module 消费、多个异步消费者、长时间事件 replay 或经过测量的 PostgreSQL queue 瓶颈出现时，重新评估 module、worker 和 broker 边界。任何调整都必须保留支付状态的单一所有者及 UNKNOWN-first 的资金安全规则。
