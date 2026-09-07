# Polymarket CLOB Go Client

面向 Polymarket CLOB API 的、上下文感知的 Go 客户端，同时也是可独立运行的、面向 Polymarket 交易系统的持久化执行服务。

## 能力特性

- L1 凭据派生与 L2 HMAC 认证
- 面向 EOA、Gnosis Safe 与 Poly1271 签名的 V2 EIP-712 订单
- 签名前的 tick size、手续费率与负风险（neg-risk）解析
- 公共市场、订单簿、定价、历史与批量订单簿查询
- 已认证的订单、余额、成交、通知、评分与撤单
- 可自动重连的已认证用户 WebSocket 数据流
- 持久化的执行意图、已签名订单恢复、到期撤单与订单状态对账
- 已认证用户流的订单与成交处理，以及持久化的仓位记账
- 面向策略的仓位特征：持仓、可用与预留份额、入场价与入场时间

## 执行运行时

`cmd/executiond` 是三部分交易程序中的执行组件：

```text
pmm market features -> strategy -> executiond -> Polymarket CLOB
                              ^             |
                              +-- position features and intent acknowledgements
```

`pmm` 负责市场特征产出并发布行情快照；策略负责解析市场标识符并发布完整的执行意图。`executiond` 负责签名、提交、撤单、已认证账户事件、持久化订单状态、仓位记账，以及使用最新行情快照为高级执行风格规划初始 child 订单。

服务使用 PostgreSQL 保存持久化状态，并使用 core NATS 传递策略消息。意图投递刻意采用 at-most-once 语义：过期或丢失的意图不会被重放。可解码且具有 `intent_id` 的非法意图会收到拒绝确认；无效 JSON 无法确认。策略必须把缺失确认视为不确定结果，而不是“未开仓”。每个钱包只能运行一个 `executiond` 实例。

### NATS 契约

完整、版本化的字段约束、示例、交付语义与兼容性规则见 [docs/protocol/nats-v1.md](docs/protocol/nats-v1.md)。策略集成应以该文档为准，而不是 import runtime Go package。

| 主题 | 方向 | 负载 | 用途 |
| --- | --- | --- | --- |
| `strategy.execution.intent` | strategy -> executiond | `ExecutionIntent` | 请求一个 `OPEN` 或 `CLOSE` 订单。 |
| `pmm.market.quotes` | market data -> executiond | `marketquotes.Snapshot` | 高级执行风格使用的最新行情快照。 |
| `execution.intent.ack` | executiond -> strategy | `ExecutionIntentAck` | 接受、拒绝、终态完成、部分成交、过期或失败。 |
| `execution.order.event` | executiond -> observers | `ExecutionOrderEvent` | 持久化的订单生命周期迁移。 |
| `position.features.<condition_id>.<token_id>` | executiond -> strategy | `PositionFeature` | 最新的持久化仓位快照。 |
| `strategy.execution.cancel` | strategy -> executiond | `ExecutionCancelRequest` | 撤销某个 intent：撤其未成交开仓单并对该 token 的整条仓位执行 `0.01 SELL FAK` 强平。 |
| `execution.cancel.ack` | executiond -> strategy | `ExecutionCancelAck` | cancel 命令的唯一终态信号。 |
| `strategy.execution.position.query` | strategy -> executiond（request/reply） | `PositionQueryRequest` | 查询当前仓位，回复发往请求的 reply subject。 |

`ExecutionIntent` 要求提供意图 ID、幂等键、策略、类型（kind）、条件 ID、token ID、结果（outcome）、方向、份额、限价、有效期限（time-in-force）、明确的下单模式以及到期时间或完成期限。调用端必须在 `policy.style` 中明确指定 `LIMIT`、`MAKER_POST_ONLY` 或 `TAKER_AGGRESSIVE`；`LIMIT` 使用意图限价直接创建初始 child，后两者会使用最新行情快照规划初始 child 的价格、post-only 与 time-in-force。当前运行时仍只创建一个 child；持续 cancel-replace、post-only crossing retry、价格漂移后的自动重报价、soft-close 或 force-close 生命周期尚未实现，对应策略字段会被拒绝。`CLOSE` 意图必须是卖出。卖出会针对可用库存进行原子预留；没有可卖仓位的平仓会以 `NO_POSITION` 拒绝。同一个条件 ID 与 token ID 同时只允许一个活跃卖出预留。

被接受的订单会在外部提交之前先被持久化。如果进程在持久化之后停止，`executiond` 会在启动时恢复 `SIGNED` 订单。结果未知的提交会依据 CLOB REST 状态进行对账。超过所配置期限的订单会被撤销。
对账循环还会从 CLOB REST 补拉账户成交，并通过与用户流相同的幂等 fill 路径修复 WebSocket 断线期间漏掉的成交。未知提交如果暂时查询不到订单，会先进入 `UNKNOWN_RECONCILE`；超过缺失订单宽限期后仍返回 404 才会标记为失败并释放预留。

`strategy.execution.cancel` 用于放弃某个 intent：executiond 会撤掉该 intent 仍挂单的 child 订单，并对其 `condition_id`/`token_id` 的整条剩余可用仓位提交一笔内部的 `0.01 SELL FAK` 强平（挂在该 intent 下的第二个 child，不产生 `execution.intent.ack`）。强平一旦派出即对策略侧视为终态：即使未成交也不再重试，剩余份额等待市场结算；命令结果通过 `execution.cancel.ack`（唯一终态信号）回报，仓位快照随后照常发布。`strategy.execution.position.query` 是 request/reply 仓位查询：不带过滤返回全部当前持仓，也可按 `condition_id` 或 `market_id` 过滤，回复负载与 `PositionFeature` 一致。

### 仓位记账

已认证的 CLOB 用户流是 `executiond` 中唯一的账户事件入口。Fill ID 使重复投递具备幂等性。成交生命周期状态（`MATCHED`、`MINED`、`CONFIRMED`、`FAILED`）会被持久化。taker BUY 会按所配置的 Polymarket 手续费公式计入净结果份额；如果该成交随后变为 `FAILED`，其仓位影响会按相同的计入份额数量被反向冲销。

发布的仓位状态是执行视图，而非结算或赎回引擎。订单状态对账由 CLOB REST 修复未知提交结果；链上结算、赎回与跨系统资金核对不属于 `executiond` 的当前职责。

### 包边界

- 仓库根目录：`executiond` 使用的 CLOB API 客户端。
- `cmd/executiond`：执行服务的 composition root。
- `internal/execution/protocol`：`executiond` 私有的 Go wire types；外部协议见 [docs/protocol/nats-v1.md](docs/protocol/nats-v1.md)。
- `pkg/accountfeed`、`pkg/executor`、`pkg/reconciler`、`pkg/store`：执行服务实现包，保持 Go 可测试性与模块化；策略集成 API 仍以 [docs/protocol/nats-v1.md](docs/protocol/nats-v1.md) 为准。

### 运行 `executiond`

`executiond` 需要常规的已认证 CLOB 环境变量（`POLYMARKET_PRIVATE_KEY`、`POLYMARKET_API_KEY`、`POLYMARKET_API_SECRET` 与 `POLYMARKET_API_PASSPHRASE`），另外还需要：

完整的环境变量说明和安全配置方式见 [docs/configuration.md](docs/configuration.md)；可使用 [.env.example](.env.example) 作为无秘密的变量名参考。

| 变量 | 必填 | 默认值 |
| --- | --- | --- |
| `EXECUTION_NATS_URL` | 是 | - |
| `EXECUTION_POSTGRES_URL` | 是 | - |
| `EXECUTION_POSITION_FEATURE_INTERVAL` | 否 | `500ms` |
| `EXECUTION_RECONCILE_INTERVAL` | 否 | `30s` |
| `EXECUTION_MISSING_ORDER_GRACE_PERIOD` | 否 | `2m` |
| `EXECUTION_CONNECT_TIMEOUT` | 否 | `10s` |
| `EXECUTION_SHUTDOWN_GRACE_PERIOD` | 否 | `10s` |

```sh
go run ./cmd/executiond
```

## 快速开始

```go
client, err := clobclient.New(clobclient.Config{
    PrivateKey: os.Getenv("POLYMARKET_PRIVATE_KEY"),
})
if err != nil { log.Fatal(err) }

credentials, err := client.DeriveCredentials(ctx)
if err != nil { log.Fatal(err) }

client, err = clobclient.New(clobclient.Config{
    PrivateKey: os.Getenv("POLYMARKET_PRIVATE_KEY"),
    Credentials: credentials,
    QPS: 10,
})
```

所有网络方法都需要 `context.Context`。订单提交从不自动重试，因为超时的请求仍可能已被接受。执行运行时通过其持久化恢复路径处理这种未知结果，而不是盲目地重发 SDK 请求。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
CLOB_TEST_PROXY=http://127.0.0.1:7890 go test -tags=integration ./integration
```

已认证的集成测试额外需要 `CLOB_TEST_PRIVATE_KEY`。它们在本地派生凭据，并且只执行只读的账户请求。
