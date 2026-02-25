package execution

import (
	"math/big"

	"github.com/shopspring/decimal"
)

// Opportunity represents a trade opportunity
type Opportunity struct {
	Symbol        string
	TokenAddress  string
	Decimals      int
	Direction     string // "BUY_DEX_SELL_CEX" or "BUY_CEX_SELL_DEX"
	CEXSymbol     string // 币安交易对, 如 "1MBABYDOGEUSDT" (已含USDT后缀)
	CEXMultiplier int64  // 币安缩写倍率: 1MBABYDOGE→1000000, 普通币→1
	PriceBuy      decimal.Decimal
	PriceSell     decimal.Decimal
	Spread        decimal.Decimal
	Amount         *big.Int        // 预期获得的代币数量 (raw units, 含 decimals)
	TradeAmountUSD decimal.Decimal // 实际交易金额 (动态计算, ≤ 配置最大值)
	MaxSlippage    decimal.Decimal // 最大可接受滑点 (如 0.005 = 0.5%)
}

// Executor defines how to execute a trade
type Executor interface {
	Execute(opp Opportunity) error
}

// InteractiveExecutor defines methods for manual/semi-auto control
type InteractiveExecutor interface {
	BuyOnDEX(opp Opportunity) (string, error)
	TransferToCEX(opp Opportunity) (string, error)
	SellOnCEX(opp Opportunity) (int64, error)
}
