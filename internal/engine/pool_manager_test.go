package engine

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
)

const (
	usdtAddr  = "0x55d398326f99059fF775485246999027B3197955"
	tokenAddr = "0x1234567890abcdef1234567890abcdef12345678"
)

// Q96 = 2^96
var q96 = new(big.Int).Exp(big.NewInt(2), big.NewInt(96), nil)

// newV3Pool creates a V3 pool for testing.
// usdtIsToken0: if true, USDT is token0 and target token is token1; otherwise reversed.
func newV3Pool(usdtIsToken0 bool, sqrtPriceX96, liquidity *big.Int, feeTier uint32, decimals int) *Pool {
	t0, t1 := common.HexToAddress(usdtAddr), common.HexToAddress(tokenAddr)
	if !usdtIsToken0 {
		t0, t1 = t1, t0
	}
	return &Pool{
		Address:      common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		Token0:       t0,
		Token1:       t1,
		Reserve0:     big.NewInt(0),
		Reserve1:     big.NewInt(0),
		Symbol:       "TEST",
		TokenAddr:    tokenAddr,
		Decimals:     decimals,
		Version:      3,
		FeeTier:      feeTier,
		SqrtPriceX96: sqrtPriceX96,
		Liquidity:    liquidity,
	}
}

// newV2Pool creates a V2 pool for testing.
func newV2Pool(usdtIsToken0 bool, reserve0, reserve1 *big.Int, decimals int) *Pool {
	t0, t1 := common.HexToAddress(usdtAddr), common.HexToAddress(tokenAddr)
	if !usdtIsToken0 {
		t0, t1 = t1, t0
	}
	return &Pool{
		Address:  common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Token0:   t0,
		Token1:   t1,
		Reserve0: reserve0,
		Reserve1: reserve1,
		Symbol:   "TEST",
		TokenAddr: tokenAddr,
		Decimals: decimals,
		Version:  2,
	}
}

// assertApprox checks that got is within tolerance of want.
func assertApprox(t *testing.T, name string, got, want, tolerance decimal.Decimal) {
	t.Helper()
	diff := got.Sub(want).Abs()
	if diff.GreaterThan(tolerance) {
		t.Errorf("%s: got %s, want ~%s (diff %s > tolerance %s)", name, got.String(), want.String(), diff.String(), tolerance.String())
	}
}

// --- calcV3AMMOutput tests ---

func TestCalcV3AMMOutput_ZeroForOne(t *testing.T) {
	// USDT=token0, price=1.0 (S=Q96), L=10^24, feeTier=2500, token 18 decimals
	// Input: 100 USDT
	// Expected: ~99.74 tokens (0.25% fee + ~0.01% slippage on 1M virtual reserve)
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil) // 10^24
	S := new(big.Int).Set(q96)                                  // price = 1.0

	pool := newV3Pool(true, S, L, 2500, 18)
	amountIn := decimal.NewFromInt(100)

	tokenOut, effPrice := pm.calcV3AMMOutput(pool, 18, amountIn)

	if tokenOut.IsZero() {
		t.Fatal("tokenOut should not be zero")
	}

	// Expected: ~99.74 (fee 0.25% = 99.75, minus tiny slippage ~0.01)
	assertApprox(t, "tokenOut", tokenOut, decimal.NewFromFloat(99.74), decimal.NewFromFloat(0.05))

	// Effective price should be slightly above 1.0 (paying ~100 USDT for ~99.74 tokens)
	if effPrice.LessThanOrEqual(decimal.NewFromInt(1)) {
		t.Errorf("effectivePrice should be > 1.0, got %s", effPrice.String())
	}
	assertApprox(t, "effectivePrice", effPrice, decimal.NewFromFloat(1.0026), decimal.NewFromFloat(0.001))
}

func TestCalcV3AMMOutput_OneForZero(t *testing.T) {
	// USDT=token1, price=1.0 (S=Q96), L=10^24, feeTier=2500, token 18 decimals
	// Due to symmetry at price=1.0, output should be same as zeroForOne
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	S := new(big.Int).Set(q96)

	pool := newV3Pool(false, S, L, 2500, 18)
	amountIn := decimal.NewFromInt(100)

	tokenOut, effPrice := pm.calcV3AMMOutput(pool, 18, amountIn)

	if tokenOut.IsZero() {
		t.Fatal("tokenOut should not be zero")
	}

	assertApprox(t, "tokenOut", tokenOut, decimal.NewFromFloat(99.74), decimal.NewFromFloat(0.05))
	assertApprox(t, "effectivePrice", effPrice, decimal.NewFromFloat(1.0026), decimal.NewFromFloat(0.001))
}

func TestCalcV3AMMOutput_HigherPrice(t *testing.T) {
	// Token costs $4 USDT, USDT=token0
	// price(token1/token0) = 0.25, sqrtPrice = 0.5, S = Q96/2
	// L=10^24, feeTier=2500, token 18 decimals
	// Input: 100 USDT → expect ~24.94 tokens ($4 per token, 0.25% fee + slippage)
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	S := new(big.Int).Div(q96, big.NewInt(2)) // S = Q96/2

	pool := newV3Pool(true, S, L, 2500, 18)
	amountIn := decimal.NewFromInt(100)

	tokenOut, effPrice := pm.calcV3AMMOutput(pool, 18, amountIn)

	if tokenOut.IsZero() {
		t.Fatal("tokenOut should not be zero")
	}

	// At $4/token: 100/4 = 25, minus 0.25% fee ≈ 24.94, minus tiny slippage
	assertApprox(t, "tokenOut", tokenOut, decimal.NewFromFloat(24.94), decimal.NewFromFloat(0.05))
	// Effective price should be close to $4
	assertApprox(t, "effectivePrice", effPrice, decimal.NewFromFloat(4.0), decimal.NewFromFloat(0.05))
}

func TestCalcV3AMMOutput_DifferentDecimals(t *testing.T) {
	// Token with 8 decimals at $100/token, USDT=token1 (oneForZero)
	// price(token1/token0) = (100 × 10^18) / (1 × 10^8) = 10^12
	// sqrt(10^12) = 10^6 ← exact, no approximation error
	// S = 10^6 × Q96
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil) // large L → negligible slippage

	S := new(big.Int).Mul(big.NewInt(1_000_000), q96) // sqrt(10^12) × Q96

	pool := newV3Pool(false, S, L, 500, 8) // feeTier=500 (0.05%)
	amountIn := decimal.NewFromInt(100)

	tokenOut, effPrice := pm.calcV3AMMOutput(pool, 8, amountIn)

	if tokenOut.IsZero() {
		t.Fatal("tokenOut should not be zero for 8-decimal token")
	}

	// At $100/token: 100 USDT buys ~1 token, minus 0.05% fee ≈ 0.9995
	assertApprox(t, "tokenOut", tokenOut, decimal.NewFromFloat(0.9995), decimal.NewFromFloat(0.001))
	assertApprox(t, "effectivePrice", effPrice, decimal.NewFromFloat(100.0), decimal.NewFromFloat(0.1))
}

func TestCalcV3AMMOutput_ZeroLiquidity(t *testing.T) {
	pm := NewPoolManager(nil)
	pool := newV3Pool(true, new(big.Int).Set(q96), big.NewInt(0), 2500, 18)

	tokenOut, _ := pm.calcV3AMMOutput(pool, 18, decimal.NewFromInt(100))
	if !tokenOut.IsZero() {
		t.Errorf("expected zero output for zero liquidity, got %s", tokenOut.String())
	}
}

func TestCalcV3AMMOutput_ZeroSqrtPrice(t *testing.T) {
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	pool := newV3Pool(true, big.NewInt(0), L, 2500, 18)

	tokenOut, _ := pm.calcV3AMMOutput(pool, 18, decimal.NewFromInt(100))
	if !tokenOut.IsZero() {
		t.Errorf("expected zero output for zero sqrtPrice, got %s", tokenOut.String())
	}
}

// --- calcV3LiquidityUSD tests ---

func TestCalcV3LiquidityUSD_Token0(t *testing.T) {
	// USDT=token0, S=Q96 (price=1), L=10^24
	// virtualUSDT = L × Q96 / S = 10^24 → 10^6 USDT (1M)
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	pool := newV3Pool(true, new(big.Int).Set(q96), L, 2500, 18)

	liq := pm.calcV3LiquidityUSD(pool)
	expected := decimal.NewFromInt(1_000_000)
	assertApprox(t, "liquidity", liq, expected, decimal.NewFromInt(1))
}

func TestCalcV3LiquidityUSD_Token1(t *testing.T) {
	// USDT=token1, S=Q96 (price=1), L=10^24
	// virtualUSDT = L × S / Q96 = 10^24 → 10^6 USDT (1M)
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	pool := newV3Pool(false, new(big.Int).Set(q96), L, 2500, 18)

	liq := pm.calcV3LiquidityUSD(pool)
	expected := decimal.NewFromInt(1_000_000)
	assertApprox(t, "liquidity", liq, expected, decimal.NewFromInt(1))
}

func TestCalcV3LiquidityUSD_HigherPrice(t *testing.T) {
	// USDT=token0, token=$4, S=Q96/2
	// virtualUSDT = L × Q96 / S = 10^24 × Q96 / (Q96/2) = 2×10^24 → 2M USDT
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	S := new(big.Int).Div(q96, big.NewInt(2))
	pool := newV3Pool(true, S, L, 2500, 18)

	liq := pm.calcV3LiquidityUSD(pool)
	expected := decimal.NewFromInt(2_000_000)
	assertApprox(t, "liquidity", liq, expected, decimal.NewFromInt(1))
}

func TestCalcV3LiquidityUSD_ZeroState(t *testing.T) {
	pm := NewPoolManager(nil)
	pool := newV3Pool(true, big.NewInt(0), big.NewInt(0), 2500, 18)
	liq := pm.calcV3LiquidityUSD(pool)
	if !liq.IsZero() {
		t.Errorf("expected zero liquidity for zero state, got %s", liq.String())
	}
}

// --- CalcAMMOutput integration tests ---

func TestCalcAMMOutput_V3PoolParticipates(t *testing.T) {
	// A token with only a V3 pool should get non-zero output
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	S := new(big.Int).Set(q96)

	pm.AddPoolV3("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		usdtAddr, tokenAddr, "TEST", tokenAddr, 18, 2500)
	pm.UpdateV3State("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", S, L, 0)

	tokenOut, effPrice := pm.CalcAMMOutput(tokenAddr, 18, decimal.NewFromInt(100))

	if tokenOut.IsZero() {
		t.Fatal("V3 pool should produce non-zero AMM output")
	}
	assertApprox(t, "tokenOut", tokenOut, decimal.NewFromFloat(99.74), decimal.NewFromFloat(0.05))
	if effPrice.IsZero() {
		t.Fatal("effectivePrice should not be zero")
	}
}

func TestCalcAMMOutput_BestPoolSelection(t *testing.T) {
	// Two pools for the same token: V2 with small reserves (worse output),
	// V3 with large liquidity (better output). CalcAMMOutput should pick V3.
	pm := NewPoolManager(nil)

	// V2 pool: 10K USDT reserves (small → more slippage)
	r0 := new(big.Int).Mul(big.NewInt(10_000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)) // 10K USDT
	r1 := new(big.Int).Set(r0) // 10K tokens (price=1)
	pm.AddPool("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		usdtAddr, tokenAddr, "TEST", tokenAddr, 18)
	pm.UpdateReserve("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", r0, r1)

	// V3 pool: L=10^24 (1M virtual reserve, much deeper)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	S := new(big.Int).Set(q96)
	pm.AddPoolV3("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		usdtAddr, tokenAddr, "TEST", tokenAddr, 18, 2500)
	pm.UpdateV3State("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", S, L, 0)

	tokenOut, _ := pm.CalcAMMOutput(tokenAddr, 18, decimal.NewFromInt(100))

	// V3 should win (deeper liquidity → more tokenOut)
	// V3 output ~99.74, V2 with 10K reserves on 100 USDT input → ~98.76 (much more slippage)
	assertApprox(t, "bestTokenOut", tokenOut, decimal.NewFromFloat(99.74), decimal.NewFromFloat(0.05))
}

func TestCalcAMMOutput_V2Fallback(t *testing.T) {
	// Only V2 pool available — should still work
	pm := NewPoolManager(nil)
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	r0 := new(big.Int).Mul(big.NewInt(1_000_000), e18) // 1M USDT
	r1 := new(big.Int).Set(r0)                          // 1M tokens
	pm.AddPool("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		usdtAddr, tokenAddr, "TEST", tokenAddr, 18)
	pm.UpdateReserve("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", r0, r1)

	tokenOut, effPrice := pm.CalcAMMOutput(tokenAddr, 18, decimal.NewFromInt(100))

	if tokenOut.IsZero() {
		t.Fatal("V2 pool should produce non-zero output")
	}
	// V2 fee is 0.25%, output should be ~99.75 minus tiny slippage
	assertApprox(t, "tokenOut", tokenOut, decimal.NewFromFloat(99.74), decimal.NewFromFloat(0.05))
	if effPrice.IsZero() {
		t.Fatal("effectivePrice should not be zero")
	}
}

// --- 超额输入: 单 tick 近似的极端情况 ---

func TestCalcV3AMMOutput_OversizedInput(t *testing.T) {
	// 小池子 (L=10^20, 虚拟储备各 100 USDT/100 tokens at price=1)
	// 输入 10000 USDT — 远超虚拟储备，真实 V3 会穿 tick
	// 单 tick 近似会高估输出，但必须满足:
	//   1. 不 panic
	//   2. tokenOut > 0
	//   3. tokenOut < 虚拟 token 储备 (不可能买光全部)
	//   4. effectivePrice > spotPrice (有滑点)
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil) // 虚拟储备 = L = 10^20 raw = 100 tokens
	S := new(big.Int).Set(q96)                                  // price = 1

	pool := newV3Pool(true, S, L, 2500, 18)
	amountIn := decimal.NewFromInt(10_000) // 100x the virtual reserve

	tokenOut, effPrice := pm.calcV3AMMOutput(pool, 18, amountIn)

	if tokenOut.IsZero() {
		t.Fatal("tokenOut should not be zero even for oversized input")
	}

	// 虚拟 token 储备 = L / 10^18 = 100 tokens
	virtualTokenReserve := decimal.NewFromInt(100)
	if tokenOut.GreaterThanOrEqual(virtualTokenReserve) {
		t.Errorf("tokenOut %s should be less than virtual reserve %s", tokenOut.String(), virtualTokenReserve.String())
	}

	// 价格影响应该巨大
	if effPrice.LessThanOrEqual(decimal.NewFromInt(1)) {
		t.Errorf("effectivePrice %s should be >> 1.0 for oversized input", effPrice.String())
	}
}

// --- FeeTier 费率影响 ---

func TestCalcV3AMMOutput_FeeTierComparison(t *testing.T) {
	// 同一池子参数，不同费率 → 输出递减
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	S := new(big.Int).Set(q96)
	amountIn := decimal.NewFromInt(100)

	feeTiers := []uint32{500, 2500, 10000}
	var outputs []decimal.Decimal

	for _, fee := range feeTiers {
		pool := newV3Pool(true, S, L, fee, 18)
		tokenOut, _ := pm.calcV3AMMOutput(pool, 18, amountIn)
		if tokenOut.IsZero() {
			t.Fatalf("tokenOut should not be zero for feeTier=%d", fee)
		}
		outputs = append(outputs, tokenOut)
	}

	// 500 (0.05%) > 2500 (0.25%) > 10000 (1%)
	for i := 0; i < len(outputs)-1; i++ {
		if !outputs[i].GreaterThan(outputs[i+1]) {
			t.Errorf("feeTier %d output (%s) should be > feeTier %d output (%s)",
				feeTiers[i], outputs[i].String(), feeTiers[i+1], outputs[i+1].String())
		}
	}

	// 验证量级: 0.05% fee → ~99.95, 0.25% → ~99.75, 1% → ~99.0
	assertApprox(t, "fee=500", outputs[0], decimal.NewFromFloat(99.95), decimal.NewFromFloat(0.02))
	assertApprox(t, "fee=2500", outputs[1], decimal.NewFromFloat(99.75), decimal.NewFromFloat(0.02))
	assertApprox(t, "fee=10000", outputs[2], decimal.NewFromFloat(99.00), decimal.NewFromFloat(0.02))
}

// --- 极端价格: big.Int 溢出安全 ---

func TestCalcV3AMMOutput_ExtremelyLowPrice(t *testing.T) {
	// Meme coin: $0.00000001 (10^-8), 18 decimals, USDT=token0
	// price(token1/token0) = 10^8 (1 USDT = 10^8 tokens)
	// sqrtPrice = sqrt(10^8) = 10^4
	// S = 10^4 × Q96
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(28), nil)
	S := new(big.Int).Mul(big.NewInt(10_000), q96)

	pool := newV3Pool(true, S, L, 2500, 18)
	amountIn := decimal.NewFromInt(100)

	tokenOut, effPrice := pm.calcV3AMMOutput(pool, 18, amountIn)

	if tokenOut.IsZero() {
		t.Fatal("tokenOut should not be zero for low-price token")
	}

	// 100 USDT at $0.00000001/token → ~10^10 tokens (minus fees)
	expectedTokens := decimal.NewFromFloat(9_975_000_000) // 9.975 × 10^9
	assertApprox(t, "tokenOut", tokenOut, expectedTokens, expectedTokens.Mul(decimal.NewFromFloat(0.001)))

	// Effective price should be ~10^-8
	if effPrice.IsZero() || effPrice.GreaterThan(decimal.NewFromFloat(0.001)) {
		t.Errorf("effectivePrice %s should be extremely small", effPrice.String())
	}
}

func TestCalcV3AMMOutput_ExtremelyHighPrice(t *testing.T) {
	// BTC-like: $100,000, 8 decimals, USDT=token1
	// price(token1/token0) = 100000 × 10^18 / 10^8 = 10^15
	// sqrtPrice = sqrt(10^15) = 10^7.5 ≈ 31622776
	// S ≈ 31622776 × Q96 — but we need exact. Use 10^15 × Q96^2, take sqrt.
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // modest L for expensive token

	// Compute S = isqrt(10^15 × Q96^2) for precision
	priceRaw := new(big.Int).Exp(big.NewInt(10), big.NewInt(15), nil)
	q96sq := new(big.Int).Mul(q96, q96)
	sSquared := new(big.Int).Mul(priceRaw, q96sq)
	S := new(big.Int).Sqrt(sSquared)

	pool := newV3Pool(false, S, L, 500, 8) // USDT=token1, fee=0.05%
	amountIn := decimal.NewFromInt(100)

	tokenOut, effPrice := pm.calcV3AMMOutput(pool, 8, amountIn)

	if tokenOut.IsZero() {
		t.Fatal("tokenOut should not be zero for high-price token")
	}

	// 100 USDT at $100K → ~0.001 tokens (minus fees)
	expectedTokens := decimal.NewFromFloat(0.000999) // ~0.001 × 0.9995
	assertApprox(t, "tokenOut", tokenOut, expectedTokens, decimal.NewFromFloat(0.0001))

	// Effective price ~100000
	assertApprox(t, "effPrice", effPrice, decimal.NewFromFloat(100_000), decimal.NewFromFloat(1000))
}

// --- V2 优于 V3 选池 ---

func TestCalcAMMOutput_V2BeatsV3(t *testing.T) {
	// V2: 巨量储备 (1M), V3: 微量流动性 (100 token 虚拟储备)
	// V2 应该赢
	pm := NewPoolManager(nil)

	// V2 pool: 1M USDT / 1M token (价格=1, 极低滑点)
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	r0 := new(big.Int).Mul(big.NewInt(1_000_000), e18)
	r1 := new(big.Int).Set(r0)
	pm.AddPool("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		usdtAddr, tokenAddr, "TEST", tokenAddr, 18)
	pm.UpdateReserve("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", r0, r1)

	// V3 pool: L=10^20 (虚拟储备仅 100 tokens at price=1)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)
	S := new(big.Int).Set(q96)
	pm.AddPoolV3("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		usdtAddr, tokenAddr, "TEST", tokenAddr, 18, 2500)
	pm.UpdateV3State("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", S, L, 0)

	// 用 V2 单独算的期望输出
	v2Only := NewPoolManager(nil)
	v2Only.AddPool("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		usdtAddr, tokenAddr, "TEST", tokenAddr, 18)
	v2Only.UpdateReserve("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		new(big.Int).Set(r0), new(big.Int).Set(r1))
	v2TokenOut, _ := v2Only.CalcAMMOutput(tokenAddr, 18, decimal.NewFromInt(100))

	// 合并池的输出应该 >= V2 输出 (选最优)
	bestTokenOut, _ := pm.CalcAMMOutput(tokenAddr, 18, decimal.NewFromInt(100))

	if bestTokenOut.LessThan(v2TokenOut) {
		t.Errorf("best pool output %s should be >= V2-only output %s", bestTokenOut.String(), v2TokenOut.String())
	}

	// V2 (1M 储备, 100 USDT 输入) 滑点极小 → ~99.74
	// V3 (100 虚拟储备, 100 USDT 输入) 滑点巨大 → 远小于 99
	// 最终应该选 V2
	assertApprox(t, "bestTokenOut=V2", bestTokenOut, decimal.NewFromFloat(99.74), decimal.NewFromFloat(0.05))
}

// --- GetPoolLiquidityUSD integration tests ---

func TestGetPoolLiquidityUSD_V3(t *testing.T) {
	pm := NewPoolManager(nil)
	L := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	S := new(big.Int).Set(q96)

	pm.AddPoolV3("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		usdtAddr, tokenAddr, "TEST", tokenAddr, 18, 2500)
	pm.UpdateV3State("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", S, L, 0)

	liq := pm.GetPoolLiquidityUSD("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if liq.IsZero() {
		t.Fatal("V3 pool liquidity should not be zero")
	}
	assertApprox(t, "liquidity", liq, decimal.NewFromInt(1_000_000), decimal.NewFromInt(1))
}

func TestGetPoolLiquidityUSD_V2(t *testing.T) {
	pm := NewPoolManager(nil)
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	r0 := new(big.Int).Mul(big.NewInt(500_000), e18) // 500K USDT (token0)
	r1 := new(big.Int).Mul(big.NewInt(100_000), e18) // 100K tokens (token1)

	pm.AddPool("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		usdtAddr, tokenAddr, "TEST", tokenAddr, 18)
	pm.UpdateReserve("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", r0, r1)

	liq := pm.GetPoolLiquidityUSD("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	expected := decimal.NewFromInt(500_000)
	assertApprox(t, "liquidity", liq, expected, decimal.NewFromFloat(0.01))
}
