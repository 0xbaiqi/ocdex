# CDEX — CEX-DEX 套利机器人

> ⚠️ **本项目仅供学习和研究使用，不构成任何投资建议。实盘交易存在资金损失风险，作者不对任何损失负责。**

Go 语言实现的高性能套利机器人，捕捉 **Binance (CEX)** 与 **PancakeSwap V2/V3 (DEX)** 之间的价差机会。

## 特性

- **事件驱动**: 监听链上 Sync/Swap 事件实时更新 DEX 价格，0 RPC 调用
- **V2 + V3 双池**: 自动选最优输出池
- **动态交易金额**: 根据池子深度和价差自动计算最优仓位，避免过大滑点
- **合约对冲模式**: DEX 买入 + CEX 合约开空并行执行，delta 中性
- **新币工具**: 带 Web UI 的单币种套利工具（HTTP + SSE，端口 8080）
- **Portfolio Margin**: 使用 Binance 统一账户 `/papi/` 端点

## 架构

**事件驱动流水线 (主程序)**:

```
BSC 链上事件 → LogWatcher → PoolManager → ArbitrageDetector → Executor
  ├─ V2 Sync(r0, r1)                          ↑
  └─ V3 Swap(..., sqrtPriceX96, liq, tick)    │
Binance WebSocket (现货 + 合约 miniTicker) ───┘
```

共 3 条 WebSocket 连接：1 BSC + 1 Binance 现货 + 1 Binance 合约。

**新币工具 (newcoin)**:

```
初始建仓 (三方对冲):
  CEX 现货买入 $N/2  +  DEX 链上买入 $N/2  +  CEX 合约开空 (全量)
  → delta neutral，TOKEN 净敞口 ≈ 0

套利方向 A (DEX 便宜): DEX 买入 → 转账到 CEX → CEX 卖出
套利方向 B (CEX 便宜): CEX 买入 → 提现到链上 → DEX 卖出
```

## 执行模式

| 模式 | 说明 |
|------|------|
| `FUTURES_HEDGE` | DEX 买入 + CEX 合约开空 → 充值 → 现货卖出 + 平空（主要模式） |
| `DEPOSIT_AND_SELL` | DEX 买入 → 充值 → 现货卖出（无对冲） |
| `INVENTORY_SPOT` | 库存现货模式 |

## DEX AMM 公式

**V2** — 常数乘积 `xy=k`，固定费率 0.25%

**V3** — 单 tick 集中流动性近似：

| 方向 | 公式 |
|------|------|
| USDT→TOKEN (zeroForOne) | `amountOut = L × A × S² / ((L×Q96 + A×S) × Q96)` |
| TOKEN→USDT (oneForZero) | `amountOut = A × Q96² × L / (S × (S×L + A×Q96))` |

其中 `A = amountIn × (1M - feeTier) / 1M`，`S = sqrtPriceX96`，`Q96 = 2^96`

## 项目结构

```
ocdex/
├── cmd/
│   ├── oocdex/           # 主程序：多币种事件驱动套利
│   ├── scanner/        # 独立代币扫描器
│   └── newcoin/        # 新币套利工具（Web UI）
├── config/
│   ├── config.go       # 配置结构体
│   └── config-pro.yaml # 配置模板（复制后填入真实值）
├── external/
│   ├── cex/            # Binance 现货 + Portfolio Margin 合约客户端
│   └── dex/            # PancakeSwap Router / Quoter 客户端
├── internal/
│   ├── engine/         # 核心引擎：池子状态、AMM 计算、套利检测
│   ├── cexstream/      # CEX WebSocket 价格缓存
│   ├── newcoin/        # 新币套利模块（策略、Server、Monitor、自动交易）
│   ├── execution/      # 实盘 / 模拟执行器
│   ├── discovery/      # 代币 & 池子发现
│   ├── registry/       # 代币注册表（CEX 倍率处理）
│   ├── capital/        # 资金管理（Redis）
│   └── storage/        # 数据持久化（MySQL）
└── pkg/
    ├── notify/         # 多渠道通知（Telegram、飞书）
    └── wallet/         # 钱包管理（助记词 → 私钥）
```

## 快速开始

### 1. 准备账号和密钥

#### Binance 交易所账号

1. 前往 [Binance](https://web3.binance.com/referral?ref=CDEXGITHUB) 注册账号并完成 KYC 实名认证
2. 进入 `账户 → API 管理`，点击 `创建 API`
3. API 权限勾选以下三项：
   - ✅ 读取（必须）
   - ✅ 现货及杠杆交易（买卖现货用）
   - ✅ 合约交易（开空单用）
4. 记录生成的 `API Key` 和 `Secret Key`，填入配置文件的 `exchange.binance.api_key` 和 `exchange.binance.secret_key`

> **FUTURES_HEDGE 模式额外要求**：需要开通 Portfolio Margin（统一账户）。在 Binance 账户页面找到「统一账户/Portfolio Margin」并申请开通。开通后合约交易走 `/papi/` 端点，无需单独设置保证金模式。
>
> **充值地址**：在 Binance 充值页面选择币种和 BSC 网络，复制充值地址填入 `execution.deposit.address`。

#### Binance DEX 链上钱包

程序需要一个 BSC 链上钱包来执行 PancakeSwap swap 交易和链上转账。

1. 前往 [Binance DEX](https://accounts.bsmkweb.cc/register?ref=CDEXGITHUB) 注册或使用任意 BSC 兼容钱包（MetaMask 等）
2. 导出钱包的 **12 位助记词**，填入配置文件的 `wallet.mnemonic`
3. 钱包中需要准备：
   - 少量 **BNB**（用于支付 Gas，建议至少 0.05 BNB）
   - 足够的 **USDT (BSC)** 作为交易本金

> ⚠️ 助记词是资金的唯一凭证，请勿泄露，本地配置文件不要上传到任何公开仓库。

#### Redis（主程序必须，newcoin 工具不需要）

Redis 用于主程序的资金并发管理，防止多笔交易同时超额占用本金。newcoin 单币种工具不依赖 Redis，可以跳过。

推荐使用 [Upstash](https://upstash.com) — 提供免费 Redis 实例，无需自建服务器：

1. 注册 Upstash 账号，点击 `Create Database`
2. 选择 `Redis`，区域选离服务器近的（如 `ap-southeast-1` 新加坡）
3. 创建完成后在 Details 页面复制：
   - `Endpoint` + `:Port` → 填入 `redis.host`（格式：`host:port`）
   - `Password` → 填入 `redis.password`

也可以用 Docker 本地启动：

```bash
docker run -d -p 6379:6379 redis:7-alpine
# 本地无密码：redis.host = "localhost:6379"，redis.password = ""
```

#### BSC RPC 节点

程序需要 BSC 的 HTTPS RPC（用于链上读取）和 WebSocket（用于监听实时事件），推荐使用 [Alchemy](https://alchemy.com)：

1. 注册 Alchemy 账号，进入 Dashboard 点击 `Create App`
2. 网络选择 `BNB Smart Chain (BSC) Mainnet`
3. 创建完成后点击 `API Key`，复制：
   - `HTTPS` 地址 → 填入 `chain.bsc.rpc`
   - `WebSocket` 地址 → 填入 `chain.bsc.ws_url`

> ⚠️ **免费套餐并发限制较低，无法满足实盘需求，必须升级到付费套餐。** WebSocket 断线会自动指数退避重连，但频繁断线会影响套利时效性。

### 2. 配置文件

```bash
cp config/config-pro.yaml config/config.yaml
# 编辑 config.yaml，填入以下字段：
# wallet.mnemonic                  BSC 钱包助记词（12 个英文单词）
# exchange.binance.api_key         Binance API Key
# exchange.binance.secret_key      Binance Secret Key
# chain.bsc.rpc                    Alchemy HTTPS RPC 地址
# chain.bsc.ws_url                 Alchemy WebSocket 地址
# execution.deposit.address        Binance BSC 网络充值地址（FUTURES_HEDGE 模式需要）
```

### 2. 编译 & 运行

```bash
# 编译所有
go build ./...

# 运行主程序（模拟模式，不发真实交易）
go run cmd/oocdex/main.go

# 运行主程序（实盘）
go run cmd/oocdex/main.go --live

# 运行新币套利工具（Web UI: http://localhost:8080）
go run cmd/newcoin/main.go

# 运行代币扫描器
go run cmd/scanner/main.go
```

### 3. 单元测试

```bash
go test ./internal/engine/ -v
```

### 4. 交叉编译（部署到 Linux）

```bash
make build-linux        # 主程序
make build-newcoin-linux  # 新币工具
```

## 配置说明

```yaml
strategy:
  trade_amount_usd: 30.0      # 最大交易金额上限（实际由解析公式动态计算）
  min_profit_usd: 0.5         # 最低净利润（含 Gas）
  slippage: 1.0               # 最大滑点 %

exchange:
  fees:
    binance:
      spot_rate: 0.00075          # 现货 0.075%
      futures_taker_rate: 0.00045 # 合约吃单 0.045%
      futures_maker_rate: 0.00018 # 合约挂单 0.018%

execution:
  mode: "FUTURES_HEDGE"

scanner:
  min_liquidity_usd: 500      # 池子最低 USDT 深度过滤

log:
  level: "info"   # debug=比价明细  trace=AMM计算详情
```

## 注意事项

- `CEXSymbol` 已含 `USDT` 后缀（如 `BNBUSDT`），不要重复拼接
- `trade_amount_usd` 是**上限**，实际金额由池子深度和价差动态决定
- V3 AMM 为单 tick 近似，大额交易穿越 tick 范围时会高估输出，链上滑点保护兜底
- Binance Portfolio Margin 统一账户无需设置 `marginType`（默认全仓）
- `CalcAMMOutput` 仅支持 USDT 直连池，WBNB 中转池返回 `(0,0)` 并降级估算

## 免责声明

本项目仅供学习和研究使用。实盘交易存在资金损失风险，作者不对任何损失负责。请在充分理解代码逻辑后自行评估风险。
