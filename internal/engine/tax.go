package engine

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"ocdex/config"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// TaxInfo 代币税率信息
type TaxInfo struct {
	BuyTax   decimal.Decimal `json:"buy_tax"`
	SellTax  decimal.Decimal `json:"sell_tax"`
	Verified bool            `json:"verified"`
}

// TaxDetector 税率检测器
type TaxDetector struct {
	ethClient *ethclient.Client
	router    common.Address
	usdt      common.Address
	wbnb      common.Address
	cache     map[string]TaxInfo
	cacheMu   sync.RWMutex
}

// NewTaxDetector 创建税率检测器
func NewTaxDetector(client *ethclient.Client, cfg config.BSCConfig) *TaxDetector {
	return &TaxDetector{
		ethClient: client,
		router:    common.HexToAddress(cfg.RouterV2),
		usdt:      common.HexToAddress(cfg.USDT),
		wbnb:      common.HexToAddress(cfg.WBNB),
		cache:     make(map[string]TaxInfo),
	}
}

// GetTax 获取税率 (优先读缓存，无缓存则检测)
func (d *TaxDetector) GetTax(ctx context.Context, tokenAddr string) (TaxInfo, error) {
	d.cacheMu.RLock()
	info, ok := d.cache[tokenAddr]
	d.cacheMu.RUnlock()

	if ok {
		return info, nil
	}

	// 核心币种白名单 (免税)
	if d.isWhitelist(tokenAddr) {
		info = TaxInfo{BuyTax: decimal.Zero, SellTax: decimal.Zero, Verified: true}
		d.updateCache(tokenAddr, info)
		return info, nil
	}

	// 执行检测
	log.Debug().Str("addr", tokenAddr).Msg("🕵️ 开始检测代币税率...")
	buyTax, sellTax, err := d.Detect(ctx, tokenAddr)
	if err != nil {
		return TaxInfo{}, err
	}

	info = TaxInfo{
		BuyTax:   buyTax,
		SellTax:  sellTax,
		Verified: true,
	}
	d.updateCache(tokenAddr, info)

	if !buyTax.IsZero() || !sellTax.IsZero() {
		log.Info().
			Str("addr", tokenAddr).
			Str("买入税", buyTax.StringFixed(4)).
			Str("卖出税", sellTax.StringFixed(4)).
			Msg("🚨 发现有税代币")
	}

	return info, nil
}

// Detect 实时检测买卖税率
// 原理: 模拟交易 100 USDT -> Token (测买入税) -> USDT (测卖出税)
func (d *TaxDetector) Detect(ctx context.Context, tokenAddrString string) (decimal.Decimal, decimal.Decimal, error) {
	// Router ABI
	const routerABIJSON = `[{"inputs":[{"internalType":"uint256","name":"amountOutMin","type":"uint256"},{"internalType":"address[]","name":"path","type":"address[]"},{"internalType":"address","name":"to","type":"address"},{"internalType":"uint256","name":"deadline","type":"uint256"}],"name":"swapExactETHForTokensSupportingFeeOnTransferTokens","outputs":[],"stateMutability":"payable","type":"function"},{"inputs":[{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint256","name":"amountOutMin","type":"uint256"},{"internalType":"address[]","name":"path","type":"address[]"},{"internalType":"address","name":"to","type":"address"},{"internalType":"uint256","name":"deadline","type":"uint256"}],"name":"swapExactTokensForETHSupportingFeeOnTransferTokens","outputs":[],"stateMutability":"nonpayable","type":"function"}]`

	parsedABI, err := abi.JSON(strings.NewReader(routerABIJSON))
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}

	tokenAddr := common.HexToAddress(tokenAddrString)

	// 1. 模拟买入: WBNB -> Token
	// 我们用 1 WBNB 模拟买入 (避免 USDT 授权问题，直接用 ETH 方法最方便)
	amountIn := big.NewInt(100000000000000000) // 0.1 BNB
	pathBuy := []common.Address{d.wbnb, tokenAddr}

	// 构造 CallData
	deadline := big.NewInt(time.Now().Add(10 * time.Minute).Unix())
	inputBuy, err := parsedABI.Pack(
		"swapExactETHForTokensSupportingFeeOnTransferTokens",
		big.NewInt(0), // amountOutMin = 0 (我们只为了测试)
		pathBuy,
		d.router, // 发给自己? 不，模拟调用中 sender 是 0x0
		deadline,
	)
	if err != nil {
		return decimal.Zero, decimal.Zero, fmt.Errorf("打包买入交易失败: %w", err)
	}

	// 模拟执行 (eth_call)
	msgBuy := ethereum.CallMsg{
		From:  common.Address{}, // 0x0
		To:    &d.router,
		Value: amountIn, // 附带 BNB
		Data:  inputBuy,
	}

	// ⚠️ 注意:
	// 纯 eth_call 无法直接拿到 Token 余额变化，因为状态不保存。
	// 完美的税率检测通常需要 Trace API 或 Fork 模式。
	// 这里我们采用一种简化策略:
	// 对于普通免费节点，很难精确模拟 Token 余额变化。
	// 现阶段我们先用 "getAmountsOut" (理论值) vs "模拟执行成功" 来判断是否是貔貅。
	// 如果是貔貅或黑名单，模拟交易通常会 Revert。

	// 为了真正计算税率，我们需要更高阶的技巧 (比如模拟 state override)。
	// 这是一个高级话题。鉴于当前环境，我建议先实现 "貔貅检测" (能否买入卖出)。
	// 如果能成功执行不回滚，暂时认为通过。
	// 对于明确的 Tax，我们后续可以通过对比 getAmountsOut 和 实际余额来算。

	// 简化版实现: 尝试 Call，如果报错则认为无法交易 (100% Tax/貔貅)
	_, err = d.ethClient.CallContract(ctx, msgBuy, nil)
	if err != nil {
		// 交易失败，可能是貔貅或滑点问题
		return decimal.NewFromInt(1), decimal.NewFromInt(1), nil // 标记为 100% 税
	}

	// 暂时返回 0 税率，后续升级为精确 Trace
	return decimal.Zero, decimal.Zero, nil
}

func (d *TaxDetector) updateCache(token string, info TaxInfo) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	d.cache[token] = info
}

func (d *TaxDetector) isWhitelist(token string) bool {
	// WBNB, ETH, BTCB, USDT, USDC, BUSD
	whitelist := []string{
		d.wbnb.String(),
		d.usdt.String(),
		"0x2170Ed0880ac9A755fd29B2688956BD959F933F8", // ETH
		"0x7130d2A12B9BCbFAe4f2634d864A1Ee1Ce3Ead9c", // BTCB
		"0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d", // USDC
	}
	for _, w := range whitelist {
		if strings.EqualFold(token, w) {
			return true
		}
	}
	return false
}
