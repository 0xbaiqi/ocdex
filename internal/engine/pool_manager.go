package engine

import (
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Pool 本地维护的流动性池状态
type Pool struct {
	Address     common.Address
	Token0      common.Address
	Token1      common.Address
	Reserve0    *big.Int // V2 only
	Reserve1    *big.Int // V2 only
	Symbol      string   // token symbol (e.g., "BNB")
	TokenAddr   string   // the non-quote token address
	Decimals    int      // token decimals
	TaxInfo     TaxInfo
	LastUpdated int64

	// V3 specific fields
	Version      int      // 2 or 3 (default 2)
	FeeTier      uint32   // V3: 100/500/2500/10000
	SqrtPriceX96 *big.Int // V3: current sqrt price
	Liquidity    *big.Int // V3: current in-range liquidity
	Tick         int32    // V3: current tick
}

// PoolManager 管理所有监听的池子
type PoolManager struct {
	pools    map[string]*Pool   // Key: PoolAddress (lowercase)
	tokenMap map[string][]*Pool // Key: TokenAddress (lowercase) -> List of Pools
	poolSymb map[string]string  // Key: PoolAddress (lowercase) -> token symbol
	mu       sync.RWMutex
	tax      *TaxDetector
}

// NewPoolManager 创建池子管理器
func NewPoolManager(taxDetector *TaxDetector) *PoolManager {
	return &PoolManager{
		pools:    make(map[string]*Pool),
		tokenMap: make(map[string][]*Pool),
		poolSymb: make(map[string]string),
		tax:      taxDetector,
	}
}

// AddPool 添加一个池子到管理列表
func (pm *PoolManager) AddPool(address, token0, token1, symbol, tokenAddr string, decimals int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	addrLower := strings.ToLower(address)
	if _, exists := pm.pools[addrLower]; exists {
		return
	}

	pool := &Pool{
		Address:   common.HexToAddress(address),
		Token0:    common.HexToAddress(token0),
		Token1:    common.HexToAddress(token1),
		Reserve0:  big.NewInt(0),
		Reserve1:  big.NewInt(0),
		Symbol:    symbol,
		TokenAddr: tokenAddr,
		Decimals:  decimals,
		Version:   2,
	}

	pm.pools[addrLower] = pool
	pm.poolSymb[addrLower] = symbol

	// 建立索引，方便通过代币查池子
	t0 := strings.ToLower(token0)
	t1 := strings.ToLower(token1)
	pm.tokenMap[t0] = append(pm.tokenMap[t0], pool)
	pm.tokenMap[t1] = append(pm.tokenMap[t1], pool)
}

// AddPoolV3 添加一个 V3 池子到管理列表
func (pm *PoolManager) AddPoolV3(address, token0, token1, symbol, tokenAddr string, decimals int, feeTier uint32) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	addrLower := strings.ToLower(address)
	if _, exists := pm.pools[addrLower]; exists {
		return
	}

	pool := &Pool{
		Address:      common.HexToAddress(address),
		Token0:       common.HexToAddress(token0),
		Token1:       common.HexToAddress(token1),
		Reserve0:     big.NewInt(0),
		Reserve1:     big.NewInt(0),
		Symbol:       symbol,
		TokenAddr:    tokenAddr,
		Decimals:     decimals,
		Version:      3,
		FeeTier:      feeTier,
		SqrtPriceX96: big.NewInt(0),
		Liquidity:    big.NewInt(0),
	}

	pm.pools[addrLower] = pool
	pm.poolSymb[addrLower] = symbol

	t0 := strings.ToLower(token0)
	t1 := strings.ToLower(token1)
	pm.tokenMap[t0] = append(pm.tokenMap[t0], pool)
	pm.tokenMap[t1] = append(pm.tokenMap[t1], pool)
}

// UpdateV3State 更新 V3 池子状态 (来自链上 Swap 事件)
func (pm *PoolManager) UpdateV3State(poolAddr string, sqrtPriceX96, liquidity *big.Int, tick int32) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pool, ok := pm.pools[strings.ToLower(poolAddr)]; ok && pool.Version == 3 {
		pool.SqrtPriceX96 = sqrtPriceX96
		pool.Liquidity = liquidity
		pool.Tick = tick
	}
}

// UpdateReserve 更新池子储备量 (来自链上 Sync 事件)
func (pm *PoolManager) UpdateReserve(poolAddr string, r0, r1 *big.Int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pool, ok := pm.pools[strings.ToLower(poolAddr)]; ok {
		pool.Reserve0 = r0
		pool.Reserve1 = r1
	}
}

// GetPool 获取池子 (通过池地址)
func (pm *PoolManager) GetPool(poolAddr string) (*Pool, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	pool, ok := pm.pools[strings.ToLower(poolAddr)]
	return pool, ok
}

// GetSymbolByPool 通过池地址获取代币符号
func (pm *PoolManager) GetSymbolByPool(poolAddr string) (string, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	sym, ok := pm.poolSymb[strings.ToLower(poolAddr)]
	return sym, ok
}

// GetPrice 计算某代币的瞬时价格 (USDT本位)
func (pm *PoolManager) GetPrice(tokenAddr string, decimals int) decimal.Decimal {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	pools, ok := pm.tokenMap[strings.ToLower(tokenAddr)]
	if !ok || len(pools) == 0 {
		return decimal.Zero
	}

	usdtAddr := "0x55d398326f99059ff775485246999027b3197955"
	wbnbAddr := "0xbb4cdb9cbd36b01bd1cbaebf2de08d9173bc095c"

	var bestPrice decimal.Decimal

	for _, pool := range pools {
		var price decimal.Decimal

		if pool.Version == 3 {
			// V3 pool: use sqrtPriceX96
			if strings.EqualFold(pool.Token0.Hex(), usdtAddr) || strings.EqualFold(pool.Token1.Hex(), usdtAddr) {
				price = pm.calcPriceFromV3Pool(pool, tokenAddr, decimals)
				log.Trace().
					Str("symbol", pool.Symbol).
					Str("pool", pool.Address.Hex()).
					Str("route", "V3-USDT直连").
					Uint32("fee", pool.FeeTier).
					Int("decimals", decimals).
					Str("price", price.String()).
					Msg("DEX 定价")
			}
		} else {
			// V2 pool: use reserves
			// 1. 直连 USDT 池
			if strings.EqualFold(pool.Token0.Hex(), usdtAddr) || strings.EqualFold(pool.Token1.Hex(), usdtAddr) {
				price = pm.calcPriceFromPool(pool, tokenAddr, decimals, usdtAddr, 18)
				log.Trace().
					Str("symbol", pool.Symbol).
					Str("pool", pool.Address.Hex()).
					Str("route", "V2-USDT直连").
					Int("decimals", decimals).
					Str("price", price.String()).
					Msg("DEX 定价")
			}

			// 2. WBNB 池 (需要 WBNB -> USDT 价格)
			if !price.IsPositive() && (strings.EqualFold(pool.Token0.Hex(), wbnbAddr) || strings.EqualFold(pool.Token1.Hex(), wbnbAddr)) {
				priceInBNB := pm.calcPriceFromPool(pool, tokenAddr, decimals, wbnbAddr, 18)
				if !strings.EqualFold(tokenAddr, wbnbAddr) {
					bnbPrice := pm.GetPrice(wbnbAddr, 18)
					price = priceInBNB.Mul(bnbPrice)
					log.Trace().
						Str("symbol", pool.Symbol).
						Str("pool", pool.Address.Hex()).
						Str("route", "V2-WBNB中转").
						Int("decimals", decimals).
						Str("priceInBNB", priceInBNB.String()).
						Str("bnbPrice", bnbPrice.String()).
						Str("price", price.String()).
						Msg("DEX 定价")
					if !bnbPrice.IsPositive() {
						price = decimal.Zero
					}
				}
			}
		}

		if price.IsPositive() && (bestPrice.IsZero() || price.LessThan(bestPrice)) {
			bestPrice = price
		}
	}

	return bestPrice
}

// GetPriceFromPool 从指定池子计算价格 (用于事件驱动回调)
func (pm *PoolManager) GetPriceFromPool(poolAddr string) decimal.Decimal {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	pool, ok := pm.pools[strings.ToLower(poolAddr)]
	if !ok {
		return decimal.Zero
	}

	return pm.GetPrice(pool.TokenAddr, pool.Decimals)
}

// calcPriceFromPool 根据池子储备计算价格
func (pm *PoolManager) calcPriceFromPool(pool *Pool, baseToken string, baseDecimals int, quoteToken string, quoteDecimals int) decimal.Decimal {
	if pool.Reserve0.Sign() == 0 || pool.Reserve1.Sign() == 0 {
		return decimal.Zero
	}

	r0 := decimal.NewFromBigInt(pool.Reserve0, 0)
	r1 := decimal.NewFromBigInt(pool.Reserve1, 0)

	var rBase, rQuote decimal.Decimal

	if strings.EqualFold(pool.Token0.Hex(), baseToken) {
		rBase = r0
		rQuote = r1
	} else {
		rBase = r1
		rQuote = r0
	}

	dBase := decimal.New(1, int32(baseDecimals))
	dQuote := decimal.New(1, int32(quoteDecimals))

	rate := rQuote.Div(dQuote).Div(rBase.Div(dBase))
	return rate
}

// calcPriceFromV3Pool 根据 V3 sqrtPriceX96 计算价格
// sqrtPriceX96 = sqrt(token1/token0) * 2^96
// price(token1/token0) = (sqrtPriceX96 / 2^96)^2
func (pm *PoolManager) calcPriceFromV3Pool(pool *Pool, tokenAddr string, decimals int) decimal.Decimal {
	if pool.SqrtPriceX96 == nil || pool.SqrtPriceX96.Sign() == 0 {
		return decimal.Zero
	}

	// sqrtPriceX96^2 = token1/token0 * 2^192
	sqrtP := decimal.NewFromBigInt(pool.SqrtPriceX96, 0)
	priceRaw := sqrtP.Mul(sqrtP) // sqrtPriceX96^2

	// Divide by 2^192
	two192 := decimal.NewFromBigInt(new(big.Int).Exp(big.NewInt(2), big.NewInt(192), nil), 0)
	priceToken1PerToken0 := priceRaw.Div(two192) // token1 per token0 in raw decimals

	usdtAddr := "0x55d398326f99059ff775485246999027b3197955"

	// Adjust for decimals: price = priceRaw * 10^(token0Decimals) / 10^(token1Decimals)
	// We need token price in USDT
	if strings.EqualFold(pool.Token0.Hex(), usdtAddr) {
		// USDT is token0 → price = 1/priceToken1PerToken0
		// token price (in USDT) = (1 / priceToken1PerToken0) * 10^(token1Decimals) / 10^(token0Decimals)
		if priceToken1PerToken0.IsZero() {
			return decimal.Zero
		}
		// Adjust: token1Decimals=tokenDecimals, token0Decimals=18(USDT)
		decAdj := decimal.New(1, int32(decimals-18))
		return decAdj.Div(priceToken1PerToken0)
	}

	if strings.EqualFold(pool.Token1.Hex(), usdtAddr) {
		// USDT is token1 → priceToken1PerToken0 is already USDT per token0
		// Adjust: token0Decimals=tokenDecimals, token1Decimals=18(USDT)
		decAdj := decimal.New(1, int32(18-decimals))
		return priceToken1PerToken0.Mul(decAdj)
	}

	return decimal.Zero
}

// GetPoolDebugInfo 返回池子的调试信息 (token0/token1/reserves)
func (pm *PoolManager) GetPoolDebugInfo(poolAddr string) string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	pool, ok := pm.pools[strings.ToLower(poolAddr)]
	if !ok {
		return "未找到"
	}

	usdtAddr := "0x55d398326f99059ff775485246999027b3197955"
	wbnbAddr := "0xbb4cdb9cbd36b01bd1cbaebf2de08d9173bc095c"

	if pool.Version == 3 {
		route := "V3-USDT直连"
		return fmt.Sprintf("%s fee=%d t0=%s t1=%s sqrtP=%s liq=%s",
			route, pool.FeeTier,
			pool.Token0.Hex()[:10]+"...",
			pool.Token1.Hex()[:10]+"...",
			pool.SqrtPriceX96.String(),
			pool.Liquidity.String(),
		)
	}

	route := "V2-未知"
	if strings.EqualFold(pool.Token0.Hex(), usdtAddr) || strings.EqualFold(pool.Token1.Hex(), usdtAddr) {
		route = "V2-USDT直连"
	} else if strings.EqualFold(pool.Token0.Hex(), wbnbAddr) || strings.EqualFold(pool.Token1.Hex(), wbnbAddr) {
		route = "V2-WBNB中转"
	}

	return fmt.Sprintf("%s t0=%s t1=%s r0=%s r1=%s",
		route,
		pool.Token0.Hex()[:10]+"...",
		pool.Token1.Hex()[:10]+"...",
		pool.Reserve0.String(),
		pool.Reserve1.String(),
	)
}

// GetPoolLiquidityUSD 获取池子的 USDT 侧流动性 (近似 TVL/2)
func (pm *PoolManager) GetPoolLiquidityUSD(poolAddr string) decimal.Decimal {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	pool, ok := pm.pools[strings.ToLower(poolAddr)]
	if !ok {
		return decimal.Zero
	}

	usdtAddr := "0x55d398326f99059ff775485246999027b3197955"

	if pool.Version == 3 {
		return pm.calcV3LiquidityUSD(pool)
	}

	// V2 USDT 直连池: 直接读 USDT reserve
	if strings.EqualFold(pool.Token0.Hex(), usdtAddr) {
		return decimal.NewFromBigInt(pool.Reserve0, -18)
	}
	if strings.EqualFold(pool.Token1.Hex(), usdtAddr) {
		return decimal.NewFromBigInt(pool.Reserve1, -18)
	}

	// WBNB 池: 暂不精确计算，返回 0 让调用方跳过
	return decimal.Zero
}

// calcV3LiquidityUSD 计算 V3 池子的虚拟 USDT 储备 (类 TVL/2)
// USDT 是 token0: virtualUSDT = L × Q96 / S / 10^18
// USDT 是 token1: virtualUSDT = L × S / Q96 / 10^18
func (pm *PoolManager) calcV3LiquidityUSD(pool *Pool) decimal.Decimal {
	if pool.SqrtPriceX96 == nil || pool.SqrtPriceX96.Sign() == 0 || pool.Liquidity == nil || pool.Liquidity.Sign() == 0 {
		return decimal.Zero
	}

	usdtAddr := "0x55d398326f99059ff775485246999027b3197955"
	usdtIsToken0 := strings.EqualFold(pool.Token0.Hex(), usdtAddr)
	usdtIsToken1 := strings.EqualFold(pool.Token1.Hex(), usdtAddr)
	if !usdtIsToken0 && !usdtIsToken1 {
		return decimal.Zero
	}

	L := new(big.Int).Set(pool.Liquidity)
	S := new(big.Int).Set(pool.SqrtPriceX96)
	Q96 := new(big.Int).Exp(big.NewInt(2), big.NewInt(96), nil)
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	var virtualUSDTRaw *big.Int

	if usdtIsToken0 {
		// virtualToken0 = L × Q96 / S
		num := new(big.Int).Mul(L, Q96)
		virtualUSDTRaw = new(big.Int).Div(num, S)
	} else {
		// virtualToken1 = L × S / Q96
		num := new(big.Int).Mul(L, S)
		virtualUSDTRaw = new(big.Int).Div(num, Q96)
	}

	// Convert from raw (18 decimals) to human-readable
	return decimal.NewFromBigInt(virtualUSDTRaw, 0).Div(decimal.NewFromBigInt(e18, 0))
}

// GetPoolLiquidityBySymbol 通过 symbol 查找池子的 USDT 流动性
func (pm *PoolManager) GetPoolLiquidityBySymbol(symbol string) decimal.Decimal {
	pm.mu.RLock()
	var poolAddr string
	for addr, sym := range pm.poolSymb {
		if sym == symbol {
			poolAddr = addr
			break
		}
	}
	pm.mu.RUnlock()

	if poolAddr == "" {
		return decimal.Zero
	}
	return pm.GetPoolLiquidityUSD(poolAddr)
}

// CalcAMMOutput 计算 USDT 直连池的 AMM 真实输出 (支持 V2 + V3)
// amountInUSD: 输入的 USDT 金额 (人类可读, 如 100.0)
// 返回: tokenOut (人类可读代币数量), effectivePrice (实际成交均价 USDT/token)
// 遍历所有 USDT 直连池 (V2+V3)，取最优输出 (最多 tokenOut)
// WBNB 路由池暂不支持，返回 (0, 0)
func (pm *PoolManager) CalcAMMOutput(tokenAddr string, decimals int, amountInUSD decimal.Decimal) (tokenOut, effectivePrice decimal.Decimal) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	pools, ok := pm.tokenMap[strings.ToLower(tokenAddr)]
	if !ok || len(pools) == 0 {
		return decimal.Zero, decimal.Zero
	}

	usdtAddr := "0x55d398326f99059ff775485246999027b3197955"

	var bestTokenOut, bestPrice decimal.Decimal

	for _, pool := range pools {
		var curTokenOut, curPrice decimal.Decimal

		if pool.Version == 3 {
			// V3: 单 tick 近似 AMM 输出
			if !strings.EqualFold(pool.Token0.Hex(), usdtAddr) && !strings.EqualFold(pool.Token1.Hex(), usdtAddr) {
				continue
			}
			curTokenOut, curPrice = pm.calcV3AMMOutput(pool, decimals, amountInUSD)
		} else {
			// V2: 常数乘积公式
			if !strings.EqualFold(pool.Token0.Hex(), usdtAddr) && !strings.EqualFold(pool.Token1.Hex(), usdtAddr) {
				continue
			}
			if pool.Reserve0.Sign() == 0 || pool.Reserve1.Sign() == 0 {
				continue
			}
			curTokenOut, curPrice = pm.calcV2AMMOutput(pool, decimals, amountInUSD)
		}

		if curTokenOut.IsPositive() && curTokenOut.GreaterThan(bestTokenOut) {
			bestTokenOut = curTokenOut
			bestPrice = curPrice
		}
	}

	return bestTokenOut, bestPrice
}

// calcV2AMMOutput 用 PancakeSwap V2 常数乘积公式计算真实输出
func (pm *PoolManager) calcV2AMMOutput(pool *Pool, decimals int, amountInUSD decimal.Decimal) (tokenOut, effectivePrice decimal.Decimal) {
	usdtAddr := "0x55d398326f99059ff775485246999027b3197955"

	// 确定 reserveIn (USDT 侧) 和 reserveOut (代币侧) — raw units
	var reserveIn, reserveOut *big.Int
	if strings.EqualFold(pool.Token0.Hex(), usdtAddr) {
		reserveIn = pool.Reserve0
		reserveOut = pool.Reserve1
	} else {
		reserveIn = pool.Reserve1
		reserveOut = pool.Reserve0
	}

	// amountIn: 人类可读 USDT → raw units (×10^18, USDT on BSC = 18 decimals)
	amountInRaw := amountInUSD.Mul(decimal.New(1, 18)).BigInt()

	// PancakeSwap V2 公式 (0.25% fee → 9975/10000):
	// amountOut = (reserveOut × amountIn × 9975) / (reserveIn × 10000 + amountIn × 9975)
	numerator := new(big.Int).Mul(reserveOut, new(big.Int).Mul(amountInRaw, big.NewInt(9975)))
	denominator := new(big.Int).Add(
		new(big.Int).Mul(reserveIn, big.NewInt(10000)),
		new(big.Int).Mul(amountInRaw, big.NewInt(9975)),
	)

	if denominator.Sign() == 0 {
		return decimal.Zero, decimal.Zero
	}

	amountOutRaw := new(big.Int).Div(numerator, denominator)

	// 转为人类可读: ÷ 10^tokenDecimals
	tokenOut = decimal.NewFromBigInt(amountOutRaw, int32(-decimals))
	if tokenOut.IsPositive() {
		effectivePrice = amountInUSD.Div(tokenOut)
	}

	log.Trace().
		Str("pool", pool.Address.Hex()).
		Int("version", pool.Version).
		Str("amountInUSD", amountInUSD.String()).
		Str("tokenOut", tokenOut.String()).
		Str("effectivePrice", effectivePrice.String()).
		Str("reserveIn", reserveIn.String()).
		Str("reserveOut", reserveOut.String()).
		Msg("V2 AMM 输出计算")

	return tokenOut, effectivePrice
}

// calcV3AMMOutput 根据 V3 集中流动性公式计算单 tick 近似 AMM 输出
// amountInUSD: 输入的 USDT 金额 (人类可读)
// 返回: tokenOut (人类可读代币数量), effectivePrice (实际成交均价 USDT/token)
//
// 数学原理 (当前 tick 范围内，V3 行为类似虚拟储备 AMM):
//
//	USDT 是 token0 (zeroForOne): 买入 token1
//	  S_new = S × L × Q96 / (L × Q96 + amountInAfterFee × S)
//	  amountOut = L × (S - S_new) / Q96
//
//	USDT 是 token1 (oneForZero): 买入 token0
//	  S_new = S + amountInAfterFee × Q96 / L
//	  amountOut = L × Q96 × (S_new - S) / (S × S_new)
func (pm *PoolManager) calcV3AMMOutput(pool *Pool, decimals int, amountInUSD decimal.Decimal) (tokenOut, effectivePrice decimal.Decimal) {
	if pool.SqrtPriceX96 == nil || pool.SqrtPriceX96.Sign() == 0 || pool.Liquidity == nil || pool.Liquidity.Sign() == 0 {
		return decimal.Zero, decimal.Zero
	}

	usdtAddr := "0x55d398326f99059ff775485246999027b3197955"

	// Determine direction
	usdtIsToken0 := strings.EqualFold(pool.Token0.Hex(), usdtAddr)
	usdtIsToken1 := strings.EqualFold(pool.Token1.Hex(), usdtAddr)
	if !usdtIsToken0 && !usdtIsToken1 {
		return decimal.Zero, decimal.Zero
	}

	// amountIn: human-readable USDT → raw units (×10^18)
	amountInRaw := amountInUSD.Mul(decimal.New(1, 18)).BigInt()

	// Apply V3 fee: amountInAfterFee = amountIn × (1_000_000 - feeTier) / 1_000_000
	feeTier := big.NewInt(int64(pool.FeeTier))
	million := big.NewInt(1_000_000)
	feeMultiplier := new(big.Int).Sub(million, feeTier)
	amountInAfterFee := new(big.Int).Mul(amountInRaw, feeMultiplier)
	amountInAfterFee.Div(amountInAfterFee, million)

	S := new(big.Int).Set(pool.SqrtPriceX96)
	L := new(big.Int).Set(pool.Liquidity)
	Q96 := new(big.Int).Exp(big.NewInt(2), big.NewInt(96), nil)

	var amountOutRaw *big.Int

	if usdtIsToken0 {
		// zeroForOne: buying token1 with USDT(token0)
		// S_new = S × L × Q96 / (L × Q96 + amountInAfterFee × S)
		// amountOut = L × (S - S_new) / Q96
		//
		// Simplified to avoid intermediate division:
		// S - S_new = A × S² / (L×Q96 + A×S)
		// amountOut = L × A × S² / ((L×Q96 + A×S) × Q96)

		LQ96 := new(big.Int).Mul(L, Q96)            // L × Q96
		aS := new(big.Int).Mul(amountInAfterFee, S)  // amountInAfterFee × S
		denom := new(big.Int).Add(LQ96, aS)          // L × Q96 + amountInAfterFee × S
		if denom.Sign() == 0 {
			return decimal.Zero, decimal.Zero
		}

		// numerator = L × amountInAfterFee × S²
		num := new(big.Int).Mul(L, amountInAfterFee)
		num.Mul(num, S)
		num.Mul(num, S)

		// full denominator = denom × Q96
		fullDenom := new(big.Int).Mul(denom, Q96)

		amountOutRaw = new(big.Int).Div(num, fullDenom)
	} else {
		// oneForZero: buying token0 with USDT(token1)
		// S_new = S + amountInAfterFee × Q96 / L
		// amountOut = L × Q96 × (S_new - S) / (S × S_new)
		//           = L × Q96 × (amountInAfterFee × Q96 / L) / (S × S_new)
		//           = amountInAfterFee × Q96^2 / (S × S_new)
		//
		// where S_new = (S × L + amountInAfterFee × Q96) / L

		SL := new(big.Int).Mul(S, L)                      // S × L
		aQ96 := new(big.Int).Mul(amountInAfterFee, Q96)   // amountInAfterFee × Q96
		sNewNumerator := new(big.Int).Add(SL, aQ96)       // S × L + amountInAfterFee × Q96 (= S_new × L)

		// amountOut = amountInAfterFee × Q96^2 / (S × S_new)
		//           = amountInAfterFee × Q96^2 × L / (S × sNewNumerator)
		Q96sq := new(big.Int).Mul(Q96, Q96)
		num := new(big.Int).Mul(amountInAfterFee, Q96sq)
		num.Mul(num, L)

		denom := new(big.Int).Mul(S, sNewNumerator)
		if denom.Sign() == 0 {
			return decimal.Zero, decimal.Zero
		}

		amountOutRaw = new(big.Int).Div(num, denom)
	}

	if amountOutRaw.Sign() <= 0 {
		return decimal.Zero, decimal.Zero
	}

	// Convert to human-readable: ÷ 10^tokenDecimals
	tokenOut = decimal.NewFromBigInt(amountOutRaw, int32(-decimals))
	if tokenOut.IsPositive() {
		effectivePrice = amountInUSD.Div(tokenOut)
	}

	log.Trace().
		Str("pool", pool.Address.Hex()).
		Uint32("fee", pool.FeeTier).
		Int("version", pool.Version).
		Str("amountInUSD", amountInUSD.String()).
		Str("tokenOut", tokenOut.String()).
		Str("effectivePrice", effectivePrice.String()).
		Str("sqrtPriceX96", pool.SqrtPriceX96.String()).
		Str("liquidity", pool.Liquidity.String()).
		Msg("V3 AMM 输出计算")

	return tokenOut, effectivePrice
}

// GetAllPoolAddresses 获取所有监听的池子地址 (用于 WebSocket 订阅)
func (pm *PoolManager) GetAllPoolAddresses() []common.Address {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	addrs := make([]common.Address, 0, len(pm.pools))
	for _, p := range pm.pools {
		addrs = append(addrs, p.Address)
	}
	return addrs
}
