package engine

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"ocdex/config"
	"ocdex/external/cex"
	"ocdex/internal/cexstream"
	"ocdex/internal/execution"
	"ocdex/internal/registry"
	"ocdex/pkg/notify"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ArbitrageDetector listens for pool update events and checks for arbitrage opportunities.
type ArbitrageDetector struct {
	cfg            config.Config
	poolManager    *PoolManager
	futuresCache   *cexstream.PriceCache // 合约价格 (用于检测机会和开空)
	spotCache      *cexstream.PriceCache // 现货价格 (用于参考)
	registry       *registry.TokenRegistry
	executor       execution.Executor
	notifier       *notify.MultiNotifier
	futuresClient  *cex.BinanceFuturesClient

	// Cooldown: prevent executing the same token too frequently
	lastExecMap map[string]time.Time
	execMu      sync.Mutex
	cooldown    time.Duration

	// Execution lock: only one trade at a time
	executing sync.Mutex
}

// NewArbitrageDetector creates a new event-driven arbitrage detector.
func NewArbitrageDetector(
	cfg config.Config,
	pm *PoolManager,
	futuresCache *cexstream.PriceCache,
	spotCache *cexstream.PriceCache,
	reg *registry.TokenRegistry,
	executor execution.Executor,
	notifier *notify.MultiNotifier,
	futuresClient *cex.BinanceFuturesClient,
) *ArbitrageDetector {
	return &ArbitrageDetector{
		cfg:           cfg,
		poolManager:   pm,
		futuresCache:  futuresCache,
		spotCache:     spotCache,
		registry:      reg,
		executor:      executor,
		notifier:      notifier,
		futuresClient: futuresClient,
		lastExecMap:   make(map[string]time.Time),
		cooldown:      30 * time.Second,
	}
}

// OnPoolUpdate is the callback for LogWatcher Sync events.
// It receives the pool address whose reserves just changed.
func (d *ArbitrageDetector) OnPoolUpdate(poolAddr string) {
	symbol, ok := d.poolManager.GetSymbolByPool(poolAddr)
	if !ok {
		log.Trace().Str("pool", poolAddr).Msg("池子未注册，跳过")
		return
	}

	token, ok := d.registry.GetToken(symbol)
	if !ok {
		log.Trace().Str("symbol", symbol).Msg("代币未注册，跳过")
		return
	}

	// DEX price (from local pool state, 0 RPC)
	dexPrice := d.poolManager.GetPrice(token.ContractAddress, token.Decimals)
	if dexPrice.IsZero() {
		log.Trace().Str("symbol", symbol).Msg("DEX 价格为零，跳过")
		return
	}

	// 用合约价格来检测机会 (因为最终是在合约上锁利)
	futuresPrice, ok := d.futuresCache.Get(symbol)
	if !ok || futuresPrice.IsZero() {
		log.Trace().Str("symbol", symbol).Str("dex", dexPrice.StringFixed(6)).Msg("无合约价格，跳过")
		return
	}

	// 顺便拿现货价格做参考日志
	spotPrice, _ := d.spotCache.Get(symbol)

	// 获取池子路由信息用于调试
	poolInfo := d.poolManager.GetPoolDebugInfo(poolAddr)

	log.Debug().
		Str("symbol", symbol).
		Str("DEX", dexPrice.String()).
		Str("合约", futuresPrice.String()).
		Str("现货", spotPrice.String()).
		Int("decimals", token.Decimals).
		Str("池子", poolInfo).
		Msg("Sync 事件 → 比价")

	d.checkArbitrage(token, futuresPrice, spotPrice, dexPrice, true)
}

// OnCEXPriceUpdate is the callback for CEX futures price changes.
// It receives the symbol (e.g., "BNB") whose futures price just changed.
func (d *ArbitrageDetector) OnCEXPriceUpdate(symbol string) {
	token, ok := d.registry.GetToken(symbol)
	if !ok {
		return
	}

	dexPrice := d.poolManager.GetPrice(token.ContractAddress, token.Decimals)
	if dexPrice.IsZero() {
		return
	}

	futuresPrice, ok := d.futuresCache.Get(symbol)
	if !ok || futuresPrice.IsZero() {
		return
	}

	spotPrice, _ := d.spotCache.Get(symbol)

	d.checkArbitrage(token, futuresPrice, spotPrice, dexPrice, false)
}

// calcNetProfit 计算给定交易金额和代币数量的净利润
// tradeAmountUSD: 投入 USDT 金额
// tokensBought: 实际获得的代币数量 (人类可读)
// futuresPrice: 合约价格 (单个代币, 已处理 CEXMultiplier)
// 返回: netProfit (净利润 USD), profitRate (收益率 %)
func (d *ArbitrageDetector) calcNetProfit(
	tradeAmountUSD, tokensBought, futuresPrice decimal.Decimal,
) (netProfit, profitRate decimal.Decimal) {
	futuresRevenue := tokensBought.Mul(futuresPrice)
	grossProfit := futuresRevenue.Sub(tradeAmountUSD)

	// 费用
	cexFeeRate := decimal.NewFromFloat(d.cfg.Strategy.CexFeeRate)
	if cexFeeRate.IsZero() {
		cexFeeRate = decimal.NewFromFloat(0.001)
	}

	// 合约手续费: 开空(taker) + 平空(taker)
	binanceFees := d.cfg.Exchange.Fees["binance"]
	futuresTakerRate := decimal.NewFromFloat(binanceFees.FuturesTakerRate)
	if futuresTakerRate.IsZero() {
		futuresTakerRate = decimal.NewFromFloat(0.00045)
	}
	futuresFee := futuresRevenue.Mul(futuresTakerRate).Mul(decimal.NewFromInt(2))
	// 现货卖出手续费 (充值后在CEX卖)
	spotSellFee := futuresRevenue.Mul(cexFeeRate)
	// DEX swap 手续费: 已包含在 AMM 公式的 9975/10000 中，不再重复计算
	// Gas
	gasCostUSD := decimal.NewFromFloat(0.15)

	netProfit = grossProfit.Sub(futuresFee).Sub(spotSellFee).Sub(gasCostUSD)
	if tradeAmountUSD.IsPositive() {
		profitRate = netProfit.Div(tradeAmountUSD).Mul(decimal.NewFromInt(100))
	}
	return netProfit, profitRate
}

// findOptimalTradeAmount 根据价差和池子深度，解析计算最优交易金额
//
// 核心思路 (两步法，无循环依赖):
//   第一步: 从价差和比例费率算出「边际利润=0」时的最大价格影响
//           maxImpact = (1+spread)×(1-CEX侧费率) / (1+DEX侧费率) - 1
//           再用精确公式反推金额: amount = poolLiquidity × (maxImpact - 0.0025)
//   第二步: 用 AMM 公式正向验证真实利润 (含 Gas 等固定成本)
func (d *ArbitrageDetector) findOptimalTradeAmount(
	token *registry.TokenInfo, futuresPrice decimal.Decimal, poolLiquidity decimal.Decimal, spread decimal.Decimal,
) (tradeUSD, tokenOut, netProfit decimal.Decimal, ok bool) {
	maxConfigTrade := decimal.NewFromFloat(d.cfg.Strategy.TradeAmountUSD)
	minProfitUSD := decimal.NewFromFloat(d.cfg.Strategy.MinProfitUSD)
	minTradeUSD := decimal.NewFromInt(5)

	// 费率 (DEX 手续费已含在 AMM 公式 9975/10000 中，不单独计算)
	cexFeeRate := decimal.NewFromFloat(d.cfg.Strategy.CexFeeRate)
	if cexFeeRate.IsZero() {
		cexFeeRate = decimal.NewFromFloat(0.001)
	}
	binanceFees := d.cfg.Exchange.Fees["binance"]
	futuresTakerRate := decimal.NewFromFloat(binanceFees.FuturesTakerRate)
	if futuresTakerRate.IsZero() {
		futuresTakerRate = decimal.NewFromFloat(0.00045)
	}

	one := decimal.NewFromInt(1)
	two := decimal.NewFromInt(2)
	ammFeeConst := decimal.NewFromFloat(25.0 / 9975.0) // PancakeSwap V2: 25/9975 ≈ 0.002506

	// ── 第一步: 解析计算最大可交易金额 ──
	// 精确公式: maxImpact = (1+spread) × (1 - futuresTaker×2 - cexFee) - 1
	// DEX 手续费已包含在 AMM 的 ammFeeConst 中，不重复扣除
	// 这是「再多一块钱就不赚」的边际零利润点 (不含 Gas 等固定成本)
	cexSideFees := futuresTakerRate.Mul(two).Add(cexFeeRate)
	maxImpact := one.Add(spread).Mul(one.Sub(cexSideFees)).Sub(one)

	if maxImpact.LessThanOrEqual(ammFeeConst) {
		return decimal.Zero, decimal.Zero, decimal.Zero, false
	}

	// 精确反推: amountIn = poolLiquidity × (maxImpact - ammFee)
	maxAmountFromPool := poolLiquidity.Mul(maxImpact.Sub(ammFeeConst))

	// 取较小值: 解析金额 vs 配置上限
	tradeUSD = maxAmountFromPool
	if maxConfigTrade.LessThan(tradeUSD) {
		tradeUSD = maxConfigTrade
	}

	if tradeUSD.LessThan(minTradeUSD) {
		return decimal.Zero, decimal.Zero, decimal.Zero, false
	}

	// ── 第二步: AMM 正向验证 (含 Gas 等固定成本) ──
	tokenOut, _ = d.poolManager.CalcAMMOutput(token.ContractAddress, token.Decimals, tradeUSD)
	if tokenOut.IsZero() {
		// WBNB 路由池: CalcAMMOutput 暂不支持，用现货价估算
		dexSpotPrice := d.poolManager.GetPrice(token.ContractAddress, token.Decimals)
		if dexSpotPrice.IsZero() {
			return decimal.Zero, decimal.Zero, decimal.Zero, false
		}
		tradeUSD = maxConfigTrade
		tokenOut = tradeUSD.Div(dexSpotPrice)
	}

	netProfit, _ = d.calcNetProfit(tradeUSD, tokenOut, futuresPrice)

	log.Trace().
		Str("token", token.Symbol).
		Str("maxImpact", maxImpact.Mul(decimal.NewFromInt(100)).StringFixed(3)+"%").
		Str("池子可交易", maxAmountFromPool.StringFixed(2)).
		Str("实际金额", tradeUSD.StringFixed(2)).
		Str("AMM输出", tokenOut.StringFixed(4)).
		Str("净利润", netProfit.StringFixed(4)).
		Msg("动态金额计算")

	if netProfit.LessThan(minProfitUSD) {
		return decimal.Zero, decimal.Zero, decimal.Zero, false
	}

	return tradeUSD, tokenOut, netProfit, true
}

// checkArbitrage evaluates if a profitable opportunity exists.
// verbose=true 打印每次比价日志 (DEX触发), verbose=false 只打印有机会时的日志 (CEX触发)
func (d *ArbitrageDetector) checkArbitrage(token *registry.TokenInfo, futuresPrice, spotPrice, dexPrice decimal.Decimal, verbose bool) {
	if dexPrice.IsZero() {
		return
	}

	// 流动性检查: 池子 USDT 深度必须达到最低要求
	minLiquidity := decimal.NewFromFloat(d.cfg.Scanner.MinLiquidityUSD)
	poolLiquidity := d.poolManager.GetPoolLiquidityBySymbol(token.Symbol)
	if minLiquidity.IsPositive() {
		if poolLiquidity.IsPositive() && poolLiquidity.LessThan(minLiquidity) {
			if verbose {
				log.Debug().
					Str("token", token.Symbol).
					Str("流动性", poolLiquidity.StringFixed(0)+" USDT").
					Str("最低要求", minLiquidity.StringFixed(0)+" USDT").
					Msg("流动性不足，跳过")
			}
			return
		}
	}

	// 处理币安缩写倍率: 1MBABYDOGE 的 CEX 价格是 100万个代币的价格
	// 需要除以倍率才能和 DEX 的单个代币价格比较
	if token.CEXMultiplier > 1 {
		m := decimal.NewFromInt(token.CEXMultiplier)
		futuresPrice = futuresPrice.Div(m)
		spotPrice = spotPrice.Div(m)
	}

	// 核心: 用合约价格算价差, 因为利润 = 合约开空价 - DEX买入价
	spread := futuresPrice.Sub(dexPrice).Div(dexPrice)
	spreadPct := spread.Mul(decimal.NewFromInt(100))

	if spread.LessThanOrEqual(decimal.Zero) {
		if verbose {
			log.Debug().
				Str("token", token.Symbol).
				Str("DEX", dexPrice.String()).
				Str("合约", futuresPrice.String()).
				Str("价差", spreadPct.StringFixed(3)+"%").
				Msg("比价结果: DEX更贵，无机会")
		}
		return
	}

	minProfitRate := decimal.NewFromFloat(d.cfg.Strategy.MinProfitRate)

	// 动态金额: 根据价差和池子深度解析计算最优交易金额
	tradeAmountUSD, tokensBought, netProfit, ok := d.findOptimalTradeAmount(token, futuresPrice, poolLiquidity, spread)
	if !ok {
		if verbose {
			log.Debug().
				Str("token", token.Symbol).
				Str("DEX", dexPrice.String()).
				Str("合约", futuresPrice.String()).
				Str("价差", spreadPct.StringFixed(3)+"%").
				Str("池子深度", poolLiquidity.StringFixed(0)+" USDT").
				Msg("比价结果: 无可盈利金额")
		}
		return
	}

	profitRate := netProfit.Div(tradeAmountUSD).Mul(decimal.NewFromInt(100))

	// 资金费率成本 (如果持仓期可能跨越结算时刻)
	futuresRevenue := tokensBought.Mul(futuresPrice)
	fundingCost := decimal.Zero
	fundingRateStr := ""
	if d.futuresClient != nil {
		fundingCost, fundingRateStr = d.estimateFundingCost(token.CEXSymbol, futuresRevenue)
	}
	netProfit = netProfit.Sub(fundingCost)
	profitRate = netProfit.Div(tradeAmountUSD).Mul(decimal.NewFromInt(100))

	// 打印盈亏明细: DEX触发每次打印, CEX触发只打印正利润
	if verbose || netProfit.IsPositive() {
		grossProfit := futuresRevenue.Sub(tradeAmountUSD)
		effectivePrice := tradeAmountUSD.Div(tokensBought)
		priceImpact := effectivePrice.Sub(dexPrice).Div(dexPrice).Mul(decimal.NewFromInt(100))
		logEvt := log.Debug().
			Str("token", token.Symbol).
			Str("DEX", dexPrice.String()).
			Str("合约", futuresPrice.String()).
			Str("现货", spotPrice.String()).
			Str("价差", spreadPct.StringFixed(3)+"%").
			Str("本金", tradeAmountUSD.StringFixed(2)+"(动态)").
			Str("价格影响", priceImpact.StringFixed(2)+"%").
			Str("毛利", grossProfit.StringFixed(4)).
			Str("净利", netProfit.StringFixed(4)).
			Str("收益率", profitRate.StringFixed(2)+"%")
		if fundingRateStr != "" {
			logEvt = logEvt.Str("费率", fundingRateStr)
		}
		logEvt.Msg("比价结果")
	}

	minProfitUSD := decimal.NewFromFloat(d.cfg.Strategy.MinProfitUSD)
	if netProfit.LessThan(minProfitUSD) {
		return
	}
	if profitRate.LessThan(minProfitRate) {
		return
	}

	// 冷却检查
	d.execMu.Lock()
	lastExec, exists := d.lastExecMap[token.Symbol]
	if exists && time.Since(lastExec) < d.cooldown {
		d.execMu.Unlock()
		return
	}
	d.lastExecMap[token.Symbol] = time.Now()
	d.execMu.Unlock()

	// 构建机会: 用 AMM 计算的真实代币输出 (以 raw units 表示)
	amountWei := tokensBought.Mul(decimal.New(1, int32(token.Decimals))).BigInt()

	// 动态滑点: 最大可接受滑点 = 净利润率的一半 (保底留一半利润)
	// 但不能超过配置的最大滑点，也不能低于 0.1%
	maxSlippage := profitRate.Div(decimal.NewFromInt(2)).Div(decimal.NewFromInt(100)) // 净利润率的一半
	configSlippage := decimal.NewFromFloat(d.cfg.Strategy.Slippage / 100)
	if maxSlippage.GreaterThan(configSlippage) {
		maxSlippage = configSlippage
	}
	minSlippage := decimal.NewFromFloat(0.001) // 0.1% 最低
	if maxSlippage.LessThan(minSlippage) {
		maxSlippage = minSlippage
	}

	opp := execution.Opportunity{
		Symbol:         token.Symbol,
		TokenAddress:   token.ContractAddress,
		Decimals:       token.Decimals,
		Direction:      "BUY_DEX_SELL_CEX",
		CEXSymbol:      token.CEXSymbol,
		CEXMultiplier:  token.CEXMultiplier,
		PriceBuy:       dexPrice,
		PriceSell:      futuresPrice,
		Spread:         spread,
		Amount:         amountWei,
		TradeAmountUSD: tradeAmountUSD,
		MaxSlippage:    maxSlippage,
	}

	msg := fmt.Sprintf(
		"发现套利机会 [%s]\n"+
			"合约: %s | 现货: %s | DEX: %s\n"+
			"价差: %s%% | 本金: $%s(动态) | 净利: $%s",
		token.Symbol,
		futuresPrice.StringFixed(4), spotPrice.StringFixed(4), dexPrice.StringFixed(4),
		spread.Mul(decimal.NewFromInt(100)).StringFixed(3),
		tradeAmountUSD.StringFixed(2),
		netProfit.StringFixed(2),
	)
	if fundingRateStr != "" {
		msg += "\n费率: " + fundingRateStr + " | 费率成本: $" + fundingCost.StringFixed(3)
	}
	msg += "\n执行中..."
	log.Info().Msg(strings.ReplaceAll(msg, "\n", " | "))
	d.notifier.Send(msg)

	go d.executeWithLock(opp)
}

// estimateFundingCost 估算持仓期间可能产生的资金费率成本
// 返回: (费率成本USD, 费率描述字符串)
// 逻辑:
//   - 查询下次结算时间，如果 < 5 分钟内，算上一次资金费率
//   - 费率为负(空头付钱) → 计为成本
//   - 费率为正(空头收钱) → 计为负成本(收益)
func (d *ArbitrageDetector) estimateFundingCost(symbol string, positionValue decimal.Decimal) (decimal.Decimal, string) {
	info, err := d.futuresClient.GetFundingInfo(symbol)
	if err != nil {
		log.Debug().Err(err).Str("symbol", symbol).Msg("获取资金费率失败")
		return decimal.Zero, ""
	}

	nextFundingTime := time.UnixMilli(info.NextFundingTime)
	timeToFunding := time.Until(nextFundingTime)
	rateStr := info.FundingRate.Mul(decimal.NewFromInt(100)).StringFixed(4) + "%"

	// 预计持仓时间约 5 分钟 (转账+确认)
	// 如果下次结算在 5 分钟内，需要考虑这笔费率
	holdDuration := 5 * time.Minute
	if timeToFunding > holdDuration {
		// 不会跨越结算时刻，无费率成本
		return decimal.Zero, rateStr + "(不跨结算)"
	}

	// 会跨越结算时刻
	// 空头: 费率为正 → 收钱(有利), 费率为负 → 付钱(成本)
	// 成本 = -费率 * 持仓价值 (负号: 空头视角, 正费率空头收钱)
	cost := info.FundingRate.Neg().Mul(positionValue)

	desc := fmt.Sprintf("%s(%.0f分钟后结算)", rateStr, timeToFunding.Minutes())
	return cost, desc
}

func (d *ArbitrageDetector) executeWithLock(opp execution.Opportunity) {
	if !d.executing.TryLock() {
		log.Warn().Str("token", opp.Symbol).Msg("执行器忙，跳过本次机会")
		return
	}
	defer d.executing.Unlock()

	if err := d.executor.Execute(opp); err != nil {
		log.Error().Err(err).Str("token", opp.Symbol).Msg("套利执行失败")
		d.notifier.Send(fmt.Sprintf("执行失败 [%s]: %v", opp.Symbol, err))
	}
}
