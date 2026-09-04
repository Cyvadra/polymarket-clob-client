# Polymarket CLOB Go Client

面向 Polymarket CLOB API 的、上下文感知的 Go 客户端，同时也是可独立运行的、面向 Polymarket 交易系统的持久化执行服务。

## 能力特性

- L1 凭据派生与 L2 HMAC 认证
- 面向 EOA、Gnosis Safe 与 Poly1271 签名的 V2 EIP-712 订单
- 签名前的 tick size、手续费率与负风险（neg-risk）解析
- 公共市场、订单簿、定价、历史与批量订单簿查询
- 已认证的订单、余额、成交、通知、评分与撤单
- 可自动重连的市场/用户 WebSocket 数据流
- 持久化的执行意图、已签名订单恢复、到期撤单与订单状态对账
- 已认证用户流的订单与成交处理，以及持久化的仓位记账
- 面向策略的仓位特征：持仓、可用与预留份额、入场价与入场时间
- 独立的 Gamma 发现与 Data API 仓位客户端，或一个聚合 `polymarket.Client`

## 执行运行时

`cmd/executiond` 是三部分交易程序中的执行组件：

```text
pmm market features -> strategy -> executiond -> Polymarket CLOB
                              ^             |
                              +-- position features and intent acknowledgements
```

`pmm` 负责市场特征产出；`executiond` 不消费 pmm 的特征。策略负责解析市场标识符并发布完整的执行意图。`executiond` 负责签名、提交、撤单、已认证账户事件、持久化订单状态与仓位记账。

服务使用 PostgreSQL 保存持久化状态，并使用 core NATS 传递策略消息。意图投递刻意采用 at-most-once 语义：过期或丢失的意图不会被重放。每个收到的意图都会在执行确认主题上得到确认，因此策略可以安全地把缺失的确认视为“未开仓”。

### NATS 契约

所有负载均使用 `schema_version: "execution.v1"`。

| 主题 | 方向 | 负载 | 用途 |
| --- | --- | --- | --- |
| `strategy.execution.intent` | strategy -> executiond | `ExecutionIntent` | 请求一个 `OPEN` 或 `CLOSE` 订单。 |
| `execution.intent.ack` | executiond -> strategy | `ExecutionIntentAck` | 接受、拒绝、终态完成、部分成交、过期或失败。 |
| `execution.order.event` | executiond -> observers | `ExecutionOrderEvent` | 持久化的订单生命周期迁移。 |
| `position.features.<condition_id>.<token_id>` | executiond -> strategy | `PositionFeature` | 最新的持久化仓位快照。 |

`ExecutionIntent` 要求提供意图 ID、幂等键、策略、类型（kind）、条件 ID、token ID、结果（outcome）、方向、份额、限价、有效期限（time-in-force）以及到期时间或完成期限。`CLOSE` 意图必须是卖出。卖出会针对可用库存进行原子预留；没有可卖仓位的平仓会以 `NO_POSITION` 拒绝。

被接受的订单会在外部提交之前先被持久化。如果进程在持久化之后停止，`executiond` 会在启动时恢复 `SIGNED` 订单。结果未知的提交会依据 CLOB REST 状态进行对账。超过所配置期限的订单会被撤销。

### 仓位记账

已认证的 CLOB 用户流是 `executiond` 中唯一的账户事件入口。Fill ID 使重复投递具备幂等性。成交生命周期状态（`MATCHED`、`MINED`、`CONFIRMED`、`FAILED`）会被持久化。taker BUY 会按所配置的 Polymarket 手续费公式计入净结果份额；如果该成交随后变为 `FAILED`，其仓位影响会按相同的计入份额数量被反向冲销。

发布的仓位状态是执行视图，而非结算或赎回引擎。其余的对账与结算工作请参见 [TODO.md](TODO.md)。

### 运行 `executiond`

`executiond` 需要常规的已认证 CLOB 环境变量（`POLYMARKET_PRIVATE_KEY`、`POLYMARKET_API_KEY`、`POLYMARKET_API_SECRET` 与 `POLYMARKET_API_PASSPHRASE`），另外还需要：

| 变量 | 必填 | 默认值 |
| --- | --- | --- |
| `EXECUTION_NATS_URL` | 是 | - |
| `EXECUTION_POSTGRES_URL` | 是 | - |
| `EXECUTION_POSITION_FEATURE_INTERVAL` | 否 | `500ms` |
| `EXECUTION_RECONCILE_INTERVAL` | 否 | `30s` |
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
