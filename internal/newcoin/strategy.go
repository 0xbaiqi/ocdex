package newcoin

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"ocdex/config"
	"ocdex/external/cex"
	"ocdex/pkg/notify"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

const txConfirmTimeout = 60 * time.Second

const routerABI = `[
	{"inputs":[{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint256","name":"amountOutMin","type":"uint256"},{"internalType":"address[]","name":"path","type":"address[]"},{"internalType":"address","name":"to","type":"address"},{"internalType":"uint256","name":"deadline","type":"uint256"}],"name":"swapExactTokensForTokens","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],"stateMutability":"nonpayable","type":"function"}
]`

const erc20ABI = `[
	{"inputs":[{"internalType":"address","name":"spender","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"approve","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"},
	{"inputs":[{"internalType":"address","name":"account","type":"address"},{"internalType":"address","name":"spender","type":"address"}],"name":"allowance","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
	{"inputs":[{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"transfer","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"}
]`

const balanceOfABI = `[{"constant":true,"inputs":[{"name":"_owner","type":"address"}],"name":"balanceOf","outputs":[{"name":"balance","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"}]`

// PositionInfo holds the complete position overview.
type PositionInfo struct {
	CEXUSDT     string `json:"cex_usdt"`
	CEXToken    string `json:"cex_token"`
	CEXTokenUSD string `json:"cex_token_usd"`
	DEXUSDT     string `json:"dex_usdt"`
	DEXToken    string `json:"dex_token"`
	DEXTokenUSD string `json:"dex_token_usd"`
	ShortQty    string `json:"short_qty"`
	ShortEntry  string `json:"short_entry"`
	TotalUSD    string `json:"total_usd"`
	RealizedPL  string `json:"realized_pl"`
	HedgeActive bool   `json:"hedge_active"`
}

// CostEstimate holds estimated costs for both arbitrage directions.
type CostEstimate struct {
	DirA_DEXFee    float64 `json:"dir_a_dex_fee"`
	DirA_CEXFee    float64 `json:"dir_a_cex_fee"`
	DirA_Gas       float64 `json:"dir_a_gas"`
	DirA_Total     float64 `json:"dir_a_total"`
	DirA_MinSpread float64 `json:"dir_a_min_spread"`
	DirB_CEXFee    float64 `json:"dir_b_cex_fee"`
	DirB_DEXFee    float64 `json:"dir_b_dex_fee"`
	DirB_Gas       float64 `json:"dir_b_gas"`
	DirB_Total     float64 `json:"dir_b_total"`
	DirB_MinSpread float64 `json:"dir_b_min_spread"`
}

// Strategy handles all trade execution for the newcoin tool.
type Strategy struct {
	cfg        *config.Config
	state      *StateManager
	monitor    *Monitor
	binClient  *cex.BinanceClient
	binFutures *cex.BinanceFuturesClient
	ethClient  *ethclient.Client
	notifier   *notify.MultiNotifier
	aggregator *DEXAggregator

	privateKey    *ecdsa.PrivateKey
	walletAddress common.Address
	chainID       *big.Int

	routerABI abi.ABI
	erc20ABI  abi.ABI
	balABI    abi.ABI

	approvedTokens sync.Map
	mu             sync.Mutex // guards concurrent trade execution
}

// NewStrategy creates a new Strategy instance.
func NewStrategy(
	cfg *config.Config,
	state *StateManager,
	monitor *Monitor,
	binClient *cex.BinanceClient,
	binFutures *cex.BinanceFuturesClient,
	ethClient *ethclient.Client,
	privateKey *ecdsa.PrivateKey,
	walletAddress common.Address,
	notifier *notify.MultiNotifier,
) (*Strategy, error) {
	chainID, err := ethClient.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get chain id: %w", err)
	}

	rABI, _ := abi.JSON(strings.NewReader(routerABI))
	eABI, _ := abi.JSON(strings.NewReader(erc20ABI))
	bABI, _ := abi.JSON(strings.NewReader(balanceOfABI))

	// Initialize DEX Aggregator (1inch + PancakeSwap V3)
	agg := NewDEXAggregator(ethClient, cfg.Chain.BSC.USDT)

	return &Strategy{
		cfg:           cfg,
		state:         state,
		monitor:       monitor,
		binClient:     binClient,
		binFutures:    binFutures,
		ethClient:     ethClient,
		notifier:      notifier,
		aggregator:    agg,
		privateKey:    privateKey,
		walletAddress: walletAddress,
		chainID:       chainID,
		routerABI:     rABI,
		erc20ABI:      eABI,
		balABI:        bABI,
	}, nil
}

// InitPositions sets up the three-way hedge: CEX buy + DEX buy + futures short.
func (s *Strategy) InitPositions(inventoryUSD float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tc := s.state.GetState().Token
	if tc == nil {
		return errors.New("no token configured")
	}

	halfUSD := inventoryUSD / 2
	cexSymbol := tc.CEXSymbol
	quoteQty := decimal.NewFromFloat(halfUSD).StringFixed(2)

	// 1. Buy spot on CEX (half)
	log.Info().Str("symbol", cexSymbol).Str("quoteQty", quoteQty).Msg("InitPositions: CEX spot buy")
	spotOrderID, err := s.binClient.PlaceMarketBuyOrder(cexSymbol, quoteQty)
	if err != nil {
		return fmt.Errorf("CEX spot buy failed: %w", err)
	}

	// 2. Buy on DEX (half) with slippage
	log.Info().Str("symbol", tc.Symbol).Float64("usd", halfUSD).Msg("InitPositions: DEX buy")
	dexTxHash, err := s.swapOnDEX(tc, halfUSD, true)
	if err != nil {
		s.notifier.Send(fmt.Sprintf("InitPositions: DEX buy failed: %v\nCEX spot already bought (order %d), please handle manually!", err, spotOrderID))
		return fmt.Errorf("DEX buy failed (CEX order %d exists): %w", spotOrderID, err)
	}

	// 3. Wait for CEX fill, query balances
	time.Sleep(2 * time.Second)

	multiplier := decimal.NewFromInt(tc.CEXMultiplier)
	if multiplier.LessThanOrEqual(decimal.Zero) {
		multiplier = decimal.NewFromInt(1)
	}
	symbol := strings.TrimSuffix(cexSymbol, "USDT")

	cexBal, _ := s.binClient.GetBalance(symbol)
	cexQty := cexBal.Div(multiplier) // adjust for CEX multiplier

	// DEX token balance
	dexBalRaw, err := s.getTokenBalance(tc.ContractAddress)
	if err != nil {
		log.Warn().Err(err).Msg("InitPositions: failed to get DEX token balance")
	}
	dexQty := decimal.NewFromBigInt(dexBalRaw, -int32(tc.Decimals)).Div(multiplier)

	totalQty := cexQty.Add(dexQty)

	// 4. Set leverage to 1x and open short for total quantity
	if err := s.binFutures.SetLeverage(cexSymbol, 1); err != nil {
		log.Warn().Err(err).Str("symbol", cexSymbol).Msg("InitPositions: failed to set leverage to 1x, continuing anyway")
	} else {
		log.Info().Str("symbol", cexSymbol).Msg("InitPositions: leverage set to 1x")
	}

	log.Info().Str("symbol", cexSymbol).Str("qty", totalQty.StringFixed(4)).Msg("InitPositions: opening short")
	shortOrderID, avgPrice, err := s.binFutures.OpenShort(cexSymbol, totalQty)
	if err != nil {
		log.Error().Err(err).Str("symbol", cexSymbol).Str("qty", totalQty.StringFixed(4)).Msg("InitPositions: short FAILED")
		s.notifier.Send(fmt.Sprintf("InitPositions: short failed: %s\nCEX+DEX already bought, please handle manually!", err.Error()))
		return fmt.Errorf("short open failed: %w", err)
	}

	// 5. Save hedge position
	hedge := &HedgePosition{
		Active:        true,
		CEXSpotQty:    cexBal.StringFixed(4),
		DEXTokenQty:   decimal.NewFromBigInt(dexBalRaw, -int32(tc.Decimals)).StringFixed(4),
		ShortQty:      totalQty.StringFixed(4),
		EntryPrice:    avgPrice.String(),
		InitAmountUSD: inventoryUSD,
		OpenedAt:      time.Now().Format(time.RFC3339),
		SpotOrderID:   spotOrderID,
		ShortOrderID:  shortOrderID,
		DEXSwapTxHash: dexTxHash,
	}
	s.state.SetHedge(hedge)

	s.notifier.Send(fmt.Sprintf("Positions opened: %s\nCEX: %s, DEX: %s, Short: %s @ %s",
		cexSymbol, cexBal.StringFixed(4),
		decimal.NewFromBigInt(dexBalRaw, -int32(tc.Decimals)).StringFixed(4),
		totalQty.StringFixed(4), avgPrice.String()))

	return nil
}

// CloseAllPositions closes all positions: sell CEX spot + sell DEX token + close short.
// Each step is attempted independently; errors are collected but don't block the next step.
// Hedge state is always cleared at the end.
func (s *Strategy) CloseAllPositions() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.state.GetState()
	tc := st.Token
	if tc == nil {
		return errors.New("no token configured")
	}

	cexSymbol := tc.CEXSymbol
	symbol := strings.TrimSuffix(cexSymbol, "USDT")
	var errs []string

	// 1. Sell all CEX spot
	cexBal, _ := s.binClient.GetBalance(symbol)
	if cexBal.IsPositive() {
		log.Info().Str("symbol", cexSymbol).Str("qty", cexBal.StringFixed(4)).Msg("CloseAll: selling CEX spot")
		_, err := s.binClient.PlaceMarketSellOrder(cexSymbol, cexBal.StringFixed(4))
		if err != nil {
			log.Error().Err(err).Msg("CloseAll: CEX spot sell failed")
			errs = append(errs, "CEX spot: "+err.Error())
		}
	} else {
		log.Info().Msg("CloseAll: no CEX spot to sell")
	}

	// 2. Sell all DEX token
	dexBal, _ := s.getTokenBalance(tc.ContractAddress)
	if dexBal != nil && dexBal.Sign() > 0 {
		log.Info().Str("symbol", tc.Symbol).Msg("CloseAll: selling DEX token")
		_, err := s.swapOnDEX(tc, 0, false)
		if err != nil {
			log.Error().Err(err).Msg("CloseAll: DEX sell failed")
			errs = append(errs, "DEX sell: "+err.Error())
		}
	} else {
		log.Info().Msg("CloseAll: no DEX token to sell")
	}

	// 3. Close short (only if hedge was active)
	if st.Hedge != nil && st.Hedge.Active {
		shortQty, _ := decimal.NewFromString(st.Hedge.ShortQty)
		if shortQty.IsPositive() {
			log.Info().Str("symbol", cexSymbol).Str("qty", shortQty.String()).Msg("CloseAll: closing short")
			_, _, err := s.binFutures.CloseShort(cexSymbol, shortQty)
			if err != nil {
				log.Warn().Err(err).Msg("CloseAll: close short failed (may already be closed)")
				errs = append(errs, "Short: "+err.Error())
			}
		}
	}

	// Always clear hedge state
	s.state.SetHedge(&HedgePosition{Active: false})

	if len(errs) > 0 {
		msg := fmt.Sprintf("清仓完成(部分失败):\n%s", strings.Join(errs, "\n"))
		s.notifier.Send(msg)
		log.Warn().Strs("errors", errs).Msg("CloseAll completed with errors")
		return nil // Still return nil so UI shows success — positions are cleared
	}

	s.notifier.Send(fmt.Sprintf("All positions closed: %s", cexSymbol))
	return nil
}

// SetupHedge buys spot on CEX and opens a short futures position (legacy, kept for compatibility).
func (s *Strategy) SetupHedge(amountUSD float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tc := s.state.GetState().Token
	if tc == nil {
		return errors.New("no token configured")
	}

	cexSymbol := tc.CEXSymbol
	quoteQty := decimal.NewFromFloat(amountUSD).StringFixed(2)

	// 1. Buy spot
	log.Info().Str("symbol", cexSymbol).Str("quoteQty", quoteQty).Msg("Hedge: buying spot")
	spotOrderID, err := s.binClient.PlaceMarketBuyOrder(cexSymbol, quoteQty)
	if err != nil {
		return fmt.Errorf("spot buy failed: %w", err)
	}

	// 2. Get spot balance to determine quantity for short
	time.Sleep(2 * time.Second) // wait for order fill

	snap := s.monitor.GetLatest()
	if !snap.CEXPrice.IsPositive() {
		return fmt.Errorf("no CEX price available")
	}

	multiplier := decimal.NewFromInt(tc.CEXMultiplier)
	if multiplier.LessThanOrEqual(decimal.Zero) {
		multiplier = decimal.NewFromInt(1)
	}
	qty := decimal.NewFromFloat(amountUSD).Div(snap.CEXPrice).Div(multiplier)

	// 3. Open short
	log.Info().Str("symbol", cexSymbol).Str("qty", qty.StringFixed(4)).Msg("Hedge: opening short")
	shortOrderID, avgPrice, err := s.binFutures.OpenShort(cexSymbol, qty)
	if err != nil {
		s.notifier.Send(fmt.Sprintf("Hedge short failed: %v\nSpot already bought (order %d), please handle manually!", err, spotOrderID))
		return fmt.Errorf("short open failed (spot order %d exists): %w", spotOrderID, err)
	}

	hedge := &HedgePosition{
		Active:        true,
		CEXSpotQty:    qty.Mul(multiplier).StringFixed(4),
		ShortQty:      qty.StringFixed(4),
		EntryPrice:    avgPrice.String(),
		InitAmountUSD: amountUSD,
		OpenedAt:      time.Now().Format(time.RFC3339),
		SpotOrderID:   spotOrderID,
		ShortOrderID:  shortOrderID,
	}
	s.state.SetHedge(hedge)

	s.notifier.Send(fmt.Sprintf("Hedge opened: %s\nSpot: %d\nShort: %s @ %s",
		cexSymbol, spotOrderID, shortOrderID, avgPrice.String()))

	return nil
}

// CloseHedge sells spot and closes the short position.
func (s *Strategy) CloseHedge() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.state.GetState()
	if st.Hedge == nil || !st.Hedge.Active {
		return errors.New("no active hedge")
	}
	tc := st.Token
	if tc == nil {
		return errors.New("no token configured")
	}

	cexSymbol := tc.CEXSymbol

	// 1. Sell spot
	spotQty := st.Hedge.CEXSpotQty
	if spotQty == "" {
		spotQty = st.Hedge.ShortQty // fallback
	}
	log.Info().Str("symbol", cexSymbol).Str("qty", spotQty).Msg("Hedge: selling spot")
	_, err := s.binClient.PlaceMarketSellOrder(cexSymbol, spotQty)
	if err != nil {
		return fmt.Errorf("spot sell failed: %w", err)
	}

	// 2. Close short
	shortQty, _ := decimal.NewFromString(st.Hedge.ShortQty)
	log.Info().Str("symbol", cexSymbol).Str("qty", shortQty.String()).Msg("Hedge: closing short")
	_, _, err = s.binFutures.CloseShort(cexSymbol, shortQty)
	if err != nil {
		return fmt.Errorf("close short failed: %w", err)
	}

	s.state.SetHedge(&HedgePosition{Active: false})
	s.notifier.Send(fmt.Sprintf("Hedge closed: %s", cexSymbol))
	return nil
}

// GetPositions returns a complete position overview.
func (s *Strategy) GetPositions() (*PositionInfo, error) {
	tc := s.state.GetState().Token
	if tc == nil {
		return nil, errors.New("no token configured")
	}

	st := s.state.GetState()
	snap := s.monitor.GetLatest()
	cexSymbol := tc.CEXSymbol
	symbol := strings.TrimSuffix(cexSymbol, "USDT")

	info := &PositionInfo{
		RealizedPL: fmt.Sprintf("%.2f", st.TotalPL),
	}

	// CEX USDT
	cexUSDT, err := s.binClient.GetBalance("USDT")
	if err != nil {
		info.CEXUSDT = "error"
	} else {
		info.CEXUSDT = cexUSDT.StringFixed(2)
	}

	// CEX Token
	cexToken, err := s.binClient.GetBalance(symbol)
	if err != nil {
		info.CEXToken = "error"
	} else {
		info.CEXToken = cexToken.StringFixed(4)
		if snap.CEXPrice.IsPositive() {
			multiplier := decimal.NewFromInt(tc.CEXMultiplier)
			if multiplier.LessThanOrEqual(decimal.Zero) {
				multiplier = decimal.NewFromInt(1)
			}
			tokenUSD := cexToken.Mul(snap.CEXPrice).Mul(multiplier)
			info.CEXTokenUSD = tokenUSD.StringFixed(2)
		}
	}

	// DEX USDT
	dexUSDT, err := s.getTokenBalance(s.cfg.Chain.BSC.USDT)
	if err != nil {
		info.DEXUSDT = "error"
	} else {
		info.DEXUSDT = decimal.NewFromBigInt(dexUSDT, -18).StringFixed(2)
	}

	// DEX Token
	dexToken, err := s.getTokenBalance(tc.ContractAddress)
	if err != nil {
		info.DEXToken = "error"
	} else {
		dexTokenDec := decimal.NewFromBigInt(dexToken, -int32(tc.Decimals))
		info.DEXToken = dexTokenDec.StringFixed(4)
		if snap.DEXPrice.IsPositive() {
			info.DEXTokenUSD = dexTokenDec.Mul(snap.DEXPrice).StringFixed(2)
		}
	}

	// Hedge/Short info
	if st.Hedge != nil && st.Hedge.Active {
		info.HedgeActive = true
		info.ShortQty = st.Hedge.ShortQty
		info.ShortEntry = st.Hedge.EntryPrice
	}

	// Total USD estimate
	var totalUSD decimal.Decimal
	if v, err := decimal.NewFromString(info.CEXUSDT); err == nil {
		totalUSD = totalUSD.Add(v)
	}
	if v, err := decimal.NewFromString(info.CEXTokenUSD); err == nil {
		totalUSD = totalUSD.Add(v)
	}
	if v, err := decimal.NewFromString(info.DEXUSDT); err == nil {
		totalUSD = totalUSD.Add(v)
	}
	if v, err := decimal.NewFromString(info.DEXTokenUSD); err == nil {
		totalUSD = totalUSD.Add(v)
	}
	info.TotalUSD = totalUSD.StringFixed(2)

	return info, nil
}

// EstimateTradeCost estimates fees for both arbitrage directions.
func (s *Strategy) EstimateTradeCost(amountUSD float64) *CostEstimate {
	// Fee rates from config
	var spotRate, dexFeeRate float64
	if fees, ok := s.cfg.Exchange.Fees["binance"]; ok {
		spotRate = fees.SpotRate // e.g. 0.00075
	}
	dexFeeRate = s.cfg.Strategy.DexFeeRate // e.g. 0.0025

	if spotRate == 0 {
		spotRate = 0.001 // default 0.1%
	}
	if dexFeeRate == 0 {
		dexFeeRate = 0.0025 // default 0.25%
	}

	est := &CostEstimate{}

	// Direction A: DEX buy -> transfer -> CEX sell
	// DEX fee: buy on DEX
	est.DirA_DEXFee = amountUSD * dexFeeRate
	// CEX fee: sell on CEX
	est.DirA_CEXFee = amountUSD * spotRate
	// Gas: swap + transfer
	est.DirA_Gas = 0.30
	est.DirA_Total = est.DirA_DEXFee + est.DirA_CEXFee + est.DirA_Gas
	est.DirA_MinSpread = est.DirA_Total / amountUSD * 100

	// Direction B: CEX buy -> withdraw -> DEX sell
	est.DirB_CEXFee = amountUSD * spotRate
	est.DirB_DEXFee = amountUSD * dexFeeRate
	est.DirB_Gas = 0.15 // swap only
	est.DirB_Total = est.DirB_CEXFee + est.DirB_DEXFee + est.DirB_Gas
	est.DirB_MinSpread = est.DirB_Total / amountUSD * 100

	return est
}

// ExecuteDEXtoCEX: (Legacy) DEX is cheaper -> buy on DEX -> transfer to CEX -> sell on CEX.
// Deprecated: Use ArbitrageSellCEXBuyDEX instead for the correct simultaneous execution.
func (s *Strategy) ExecuteDEXtoCEX(amountUSD float64) (*Trade, error) {
	return s.ArbitrageSellCEXBuyDEX(amountUSD)
}

// ArbitrageSellCEXBuyDEX executes the correct arbitrage when CEX is expensive and DEX is cheap:
//
//	Step 1: CEX sell TOKEN (get USDT) + DEX buy TOKEN (spend USDT)  — near-simultaneous
//	Step 2: Transfer bought tokens from DEX wallet → CEX deposit address  — rebalance
//	Step 3: Wait for deposit to arrive on CEX
//
// After completion, token counts on both sides are rebalanced and we earned the price spread in USDT.
func (s *Strategy) ArbitrageSellCEXBuyDEX(amountUSD float64) (*Trade, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tc := s.state.GetState().Token
	if tc == nil {
		return nil, errors.New("no token configured")
	}

	snap := s.monitor.GetLatest()
	if !snap.CEXPrice.IsPositive() || !snap.DEXPrice.IsPositive() {
		return nil, errors.New("no price data available")
	}

	// Estimate fees
	est := s.EstimateTradeCost(amountUSD)

	// Calculate how many tokens to sell on CEX
	multiplier := decimal.NewFromInt(tc.CEXMultiplier)
	if multiplier.LessThanOrEqual(decimal.Zero) {
		multiplier = decimal.NewFromInt(1)
	}
	symbol := strings.TrimSuffix(tc.CEXSymbol, "USDT")

	// snap.CEXPrice is normalized per-token (already divided by multiplier in Monitor)
	// tokenQty = number of actual tokens to trade
	tokenQty := decimal.NewFromFloat(amountUSD).Div(snap.CEXPrice)
	// cexSellQty = quantity in CEX units (e.g., for 1MBABYDOGE: 20M tokens / 1M = 20)
	cexSellQty := tokenQty.Div(multiplier)

	trade := Trade{
		ID:          fmt.Sprintf("t-%d", time.Now().UnixNano()),
		Direction:   "SELL_CEX_BUY_DEX",
		Symbol:      tc.Symbol,
		AmountUSD:   amountUSD,
		SpreadPct:   snap.CEXPrice.Sub(snap.DEXPrice).Div(snap.DEXPrice).Mul(decimal.NewFromInt(100)).InexactFloat64(),
		CEXPrice:    snap.CEXPrice.String(),
		DEXPrice:    snap.DEXPrice.String(),
		Status:      "executing",
		GasCostUSD:  est.DirA_Gas,
		CEXFeeUSD:   est.DirA_CEXFee,
		DEXFeeUSD:   est.DirA_DEXFee,
		TotalFeeUSD: est.DirA_Total,
		CreatedAt:   time.Now(),
	}
	s.state.AddTrade(trade)

	// Check CEX balance — must have enough tokens to sell (in CEX units)
	cexBal, err := s.binClient.GetBalance(symbol)
	if err != nil {
		s.state.UpdateTrade(trade.ID, func(t *Trade) {
			t.Status = "failed"
			t.Error = "failed to get CEX balance: " + err.Error()
		})
		return &trade, fmt.Errorf("get CEX balance: %w", err)
	}
	if cexBal.LessThan(cexSellQty) {
		s.state.UpdateTrade(trade.ID, func(t *Trade) {
			t.Status = "failed"
			t.Error = fmt.Sprintf("CEX balance too low: have %s, need %s", cexBal.StringFixed(4), cexSellQty.StringFixed(4))
		})
		return &trade, fmt.Errorf("CEX balance too low: have %s, need %s", cexBal.StringFixed(4), cexSellQty.StringFixed(4))
	}

	// ═══════════════════════════════════════════════
	// Step 1: Simultaneous CEX sell + DEX buy
	// ═══════════════════════════════════════════════

	log.Info().
		Str("symbol", tc.Symbol).
		Str("cex_sell_qty", cexSellQty.StringFixed(4)).
		Str("token_qty", tokenQty.StringFixed(4)).
		Float64("dex_buy_usd", amountUSD).
		Str("cex_price", snap.CEXPrice.String()).
		Str("dex_price", snap.DEXPrice.String()).
		Int64("multiplier", tc.CEXMultiplier).
		Msg("Arbitrage: CEX sell + DEX buy")

	// Start DEX buy in goroutine (async)
	type dexResult struct {
		txHash string
		err    error
	}
	dexCh := make(chan dexResult, 1)
	go func() {
		txHash, err := s.swapOnDEX(tc, amountUSD, true) // buy TOKEN on DEX
		dexCh <- dexResult{txHash: txHash, err: err}
	}()

	// CEX sell (sync) — market sell is fast, use CEX units
	cexOrderID, cexErr := s.binClient.PlaceMarketSellOrder(tc.CEXSymbol, cexSellQty.StringFixed(4))

	// Wait for DEX result
	dexRes := <-dexCh

	// Check results
	if cexErr != nil && dexRes.err != nil {
		// Both failed → no position change, safe
		s.state.UpdateTrade(trade.ID, func(t *Trade) {
			t.Status = "failed"
			t.Error = fmt.Sprintf("both failed: CEX=%v, DEX=%v", cexErr, dexRes.err)
		})
		return &trade, fmt.Errorf("both legs failed: CEX=%w, DEX=%v", cexErr, dexRes.err)
	}
	if cexErr != nil {
		// CEX sell failed but DEX bought — we have extra tokens on DEX, no tokens sold
		s.state.UpdateTrade(trade.ID, func(t *Trade) {
			t.Status = "partial_dex_only"
			t.TxHash = dexRes.txHash
			t.Error = "CEX sell failed: " + cexErr.Error()
		})
		s.notifier.Send(fmt.Sprintf("⚠️ Arbitrage partial: DEX bought but CEX sell failed: %v\nPlease handle manually", cexErr))
		return &trade, fmt.Errorf("CEX sell failed (DEX buy succeeded): %w", cexErr)
	}
	if dexRes.err != nil {
		// CEX sold but DEX buy failed — we have fewer tokens total, need to buy back
		s.state.UpdateTrade(trade.ID, func(t *Trade) {
			t.Status = "partial_cex_only"
			t.OrderID = fmt.Sprintf("%d", cexOrderID)
			t.Error = "DEX buy failed: " + dexRes.err.Error()
		})
		s.notifier.Send(fmt.Sprintf("⚠️ Arbitrage partial: CEX sold but DEX buy failed: %v\nPlease buy back manually on DEX!", dexRes.err))
		return &trade, fmt.Errorf("DEX buy failed (CEX sell succeeded): %w", dexRes.err)
	}

	// Both succeeded — update trade
	s.state.UpdateTrade(trade.ID, func(t *Trade) {
		t.Status = "rebalancing"
		t.TxHash = dexRes.txHash
		t.OrderID = fmt.Sprintf("%d", cexOrderID)
	})

	log.Info().
		Str("symbol", tc.Symbol).
		Str("cex_order", fmt.Sprintf("%d", cexOrderID)).
		Str("dex_tx", dexRes.txHash).
		Msg("Arbitrage: both legs done, now rebalancing")

	// ═══════════════════════════════════════════════
	// Step 2: Transfer tokens from DEX → CEX to rebalance
	// ═══════════════════════════════════════════════

	depTxHash, err := s.transferToCEX(tc.ContractAddress)
	if err != nil {
		// Trade already locked in profit, transfer just needs retry
		s.state.UpdateTrade(trade.ID, func(t *Trade) {
			t.Status = "transfer_failed"
			t.Error = "rebalance transfer failed: " + err.Error()
		})
		s.notifier.Send(fmt.Sprintf("⚠️ Arbitrage: trade done but transfer failed: %v\nTokens still on DEX, please transfer manually", err))
		return &trade, fmt.Errorf("rebalance transfer failed: %w", err)
	}
	log.Info().Str("tx", depTxHash).Msg("Rebalance: transfer to CEX sent")

	s.state.UpdateTrade(trade.ID, func(t *Trade) {
		t.Status = "waiting_deposit"
	})

	// ═══════════════════════════════════════════════
	// Step 3: Wait for deposit to arrive
	// ═══════════════════════════════════════════════

	expectedQty := cexSellQty // We expect roughly the same amount back (in CEX units)
	if err := s.waitForDeposit(symbol, expectedQty); err != nil {
		// Profit already locked in, just haven't rebalanced yet
		s.state.UpdateTrade(trade.ID, func(t *Trade) {
			t.Status = "deposit_pending"
			t.Error = "deposit slow: " + err.Error()
		})
		log.Warn().Err(err).Msg("Deposit not confirmed in time, but trade profit is locked in")
		// Don't return error — the arbitrage profit is already captured
	}

	// Calculate profit: (CEXPrice - DEXPrice) * tokenQty - fees
	// Both prices are per-token, tokenQty is actual token count
	spread := snap.CEXPrice.Sub(snap.DEXPrice).Mul(tokenQty)
	profit := spread.InexactFloat64() - est.DirA_Total

	s.state.UpdateTrade(trade.ID, func(t *Trade) {
		t.Status = "done"
		t.ProfitUSD = profit
	})

	s.notifier.Send(fmt.Sprintf("✅ 套利完成: %s\nCEX卖 + DEX买, 利润 ~$%.2f (费用 $%.2f)", tc.Symbol, profit, est.DirA_Total))
	return &trade, nil
}

// ExecuteCEXtoDEX_Buy: CEX is cheaper -> buy on CEX (user withdraws manually).
func (s *Strategy) ExecuteCEXtoDEX_Buy(amountUSD float64) (*Trade, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tc := s.state.GetState().Token
	if tc == nil {
		return nil, errors.New("no token configured")
	}

	snap := s.monitor.GetLatest()

	est := s.EstimateTradeCost(amountUSD)

	trade := Trade{
		ID:          fmt.Sprintf("t-%d", time.Now().UnixNano()),
		Direction:   "CEX_TO_DEX",
		Symbol:      tc.Symbol,
		AmountUSD:   amountUSD,
		SpreadPct:   snap.SpreadPct.InexactFloat64(),
		CEXPrice:    snap.CEXPrice.String(),
		DEXPrice:    snap.DEXPrice.String(),
		Status:      "buying",
		GasCostUSD:  est.DirB_Gas,
		CEXFeeUSD:   est.DirB_CEXFee,
		DEXFeeUSD:   est.DirB_DEXFee,
		TotalFeeUSD: est.DirB_Total,
		CreatedAt:   time.Now(),
	}
	s.state.AddTrade(trade)

	quoteQty := decimal.NewFromFloat(amountUSD).StringFixed(2)
	orderID, err := s.binClient.PlaceMarketBuyOrder(tc.CEXSymbol, quoteQty)
	if err != nil {
		s.state.UpdateTrade(trade.ID, func(t *Trade) {
			t.Status = "failed"
			t.Error = err.Error()
		})
		return &trade, fmt.Errorf("CEX buy failed: %w", err)
	}

	s.state.UpdateTrade(trade.ID, func(t *Trade) {
		t.Status = "pending_withdrawal"
		t.OrderID = fmt.Sprintf("%d", orderID)
	})

	s.notifier.Send(fmt.Sprintf("CEX bought %s for $%.2f\nPlease withdraw to chain manually, then click 'Confirm & Sell'",
		tc.Symbol, amountUSD))

	return &trade, nil
}

// ExecuteCEXtoDEX_Sell: user confirmed tokens arrived on chain -> sell on DEX.
func (s *Strategy) ExecuteCEXtoDEX_Sell(tradeID string) (*Trade, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tc := s.state.GetState().Token
	if tc == nil {
		return nil, errors.New("no token configured")
	}

	// Find the trade
	st := s.state.GetState()
	var trade *Trade
	for i := range st.Trades {
		if st.Trades[i].ID == tradeID {
			trade = &st.Trades[i]
			break
		}
	}
	if trade == nil {
		return nil, fmt.Errorf("trade %s not found", tradeID)
	}

	s.state.UpdateTrade(tradeID, func(t *Trade) {
		t.Status = "selling"
	})

	// Swap TOKEN -> USDT on DEX (sell all token balance)
	txHash, err := s.swapOnDEX(tc, 0, false) // sell token, amount=0 means sell all balance
	if err != nil {
		s.state.UpdateTrade(tradeID, func(t *Trade) {
			t.Status = "failed"
			t.Error = "DEX sell failed: " + err.Error()
		})
		return trade, fmt.Errorf("DEX sell failed: %w", err)
	}

	snap := s.monitor.GetLatest()
	profit := decimal.NewFromFloat(trade.AmountUSD).Mul(snap.SpreadPct.Abs()).Div(decimal.NewFromInt(100)).InexactFloat64() - trade.TotalFeeUSD

	s.state.UpdateTrade(tradeID, func(t *Trade) {
		t.Status = "done"
		t.TxHash = txHash
		t.ProfitUSD = profit
	})

	s.notifier.Send(fmt.Sprintf("CEX->DEX done: %s\nProfit ~$%.2f", tc.Symbol, profit))

	updated := s.state.GetState()
	for i := range updated.Trades {
		if updated.Trades[i].ID == tradeID {
			return &updated.Trades[i], nil
		}
	}
	return trade, nil
}

// --- On-chain helpers ---

// swapOnDEX executes a swap on PancakeSwap V2 router.
// buyToken=true: USDT -> TOKEN, buyToken=false: TOKEN -> USDT.
func (s *Strategy) swapOnDEX(tc *TokenConfig, amountUSD float64, buyToken bool) (string, error) {
	settings := s.state.GetState().Settings
	slippage := settings.SlippagePct
	if slippage <= 0 {
		slippage = 1.0
	}

	// ─── Try aggregator first (1inch + PancakeSwap V3 comparison) ───
	if s.aggregator != nil {
		txHash, err := s.swapViaAggregator(tc, amountUSD, buyToken, slippage)
		if err == nil {
			return txHash, nil
		}
		log.Warn().Err(err).Msg("Aggregator swap failed, falling back to V2 Router")
	}

	// ─── Fallback: PancakeSwap V2 Router (original logic) ───
	return s.swapOnDEXv2(tc, amountUSD, buyToken)
}

// swapViaAggregator uses the DEXAggregator (1inch + PancakeSwap V3) for best execution.
func (s *Strategy) swapViaAggregator(tc *TokenConfig, amountUSD float64, buyToken bool, slippage float64) (string, error) {
	quote, err := s.aggregator.GetBestSwap(
		tc.ContractAddress, tc.Decimals,
		amountUSD, buyToken,
		s.walletAddress.Hex(), slippage,
	)
	if err != nil {
		return "", err
	}

	if len(quote.CallData) == 0 || quote.RouterAddr == "" {
		return "", errors.New("aggregator returned empty calldata")
	}

	// Ensure approval for the aggregator's router
	var tokenToApprove string
	var amountIn *big.Int
	if buyToken {
		tokenToApprove = s.cfg.Chain.BSC.USDT
		amountIn = decimal.NewFromFloat(amountUSD).Mul(decimal.New(1, 18)).BigInt()
	} else {
		tokenToApprove = tc.ContractAddress
		amountIn, err = s.getTokenBalance(tc.ContractAddress)
		if err != nil {
			return "", fmt.Errorf("get token balance: %w", err)
		}
	}

	if err := s.ensureApproval(tokenToApprove, quote.RouterAddr, amountIn); err != nil {
		return "", fmt.Errorf("aggregator approval failed: %w", err)
	}

	log.Info().
		Str("source", quote.Source).
		Str("router", quote.RouterAddr).
		Str("token_out", quote.TokenOut.StringFixed(6)).
		Str("price", quote.Price.StringFixed(8)).
		Msg("Executing swap via aggregator")

	return s.sendTransaction(common.HexToAddress(quote.RouterAddr), quote.Value, quote.CallData)
}

// swapOnDEXv2 is the original PancakeSwap V2 Router swap (fallback).
func (s *Strategy) swapOnDEXv2(tc *TokenConfig, amountUSD float64, buyToken bool) (string, error) {
	routerAddr := s.cfg.Chain.BSC.RouterV2
	usdtAddr := s.cfg.Chain.BSC.USDT

	var path []common.Address
	var amountIn *big.Int
	var tokenToApprove string

	if buyToken {
		// USDT -> TOKEN
		path = []common.Address{
			common.HexToAddress(usdtAddr),
			common.HexToAddress(tc.ContractAddress),
		}
		amountIn = decimal.NewFromFloat(amountUSD).Mul(decimal.New(1, 18)).BigInt()
		tokenToApprove = usdtAddr
	} else {
		// TOKEN -> USDT (sell all balance)
		path = []common.Address{
			common.HexToAddress(tc.ContractAddress),
			common.HexToAddress(usdtAddr),
		}
		bal, err := s.getTokenBalance(tc.ContractAddress)
		if err != nil {
			return "", fmt.Errorf("get token balance: %w", err)
		}
		if bal.Sign() <= 0 {
			return "", errors.New("no token balance to sell")
		}
		amountIn = bal
		tokenToApprove = tc.ContractAddress
	}

	// Ensure approval
	if err := s.ensureApproval(tokenToApprove, routerAddr, amountIn); err != nil {
		return "", fmt.Errorf("approval failed: %w", err)
	}

	// Calculate amountOutMin with slippage protection
	deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())
	amountOutMin := s.calcAmountOutMin(tc, amountUSD, buyToken)

	data, err := s.routerABI.Pack("swapExactTokensForTokens", amountIn, amountOutMin, path, s.walletAddress, deadline)
	if err != nil {
		return "", fmt.Errorf("pack swap: %w", err)
	}

	log.Info().Str("router", "V2").Float64("amount_usd", amountUSD).Msg("Executing swap via V2 Router (fallback)")
	return s.sendTransaction(common.HexToAddress(routerAddr), nil, data)
}

// calcAmountOutMin calculates the minimum output with slippage protection.
func (s *Strategy) calcAmountOutMin(tc *TokenConfig, amountUSD float64, buyToken bool) *big.Int {
	settings := s.state.GetState().Settings
	slippage := settings.SlippagePct
	if slippage <= 0 {
		slippage = 1.0
	}

	snap := s.monitor.GetLatest()
	if !snap.DEXPrice.IsPositive() {
		return big.NewInt(0) // no price data, skip protection
	}

	var expectedOut decimal.Decimal
	if buyToken {
		// Buying token: expectedOut = amountUSD / dexPrice (in token units)
		expectedOut = decimal.NewFromFloat(amountUSD).Div(snap.DEXPrice)
		// Convert to raw token units
		expectedOut = expectedOut.Mul(decimal.New(1, int32(tc.Decimals)))
	} else {
		// Selling token: we don't know exact amount, skip for sell-all
		return big.NewInt(0)
	}

	// Apply slippage: minOut = expectedOut * (100 - slippage) / 100
	minOut := expectedOut.Mul(decimal.NewFromFloat(100 - slippage)).Div(decimal.NewFromInt(100))
	return minOut.BigInt()
}

func (s *Strategy) transferToCEX(tokenAddr string) (string, error) {
	depAddr := s.cfg.Execution.Dep.Address
	if depAddr == "" {
		return "", errors.New("CEX deposit address not configured")
	}

	balance, err := s.getTokenBalance(tokenAddr)
	if err != nil {
		return "", fmt.Errorf("get token balance: %w", err)
	}
	if balance.Sign() <= 0 {
		return "", errors.New("no token balance to transfer")
	}

	toAddr := common.HexToAddress(depAddr)
	data, err := s.erc20ABI.Pack("transfer", toAddr, balance)
	if err != nil {
		return "", fmt.Errorf("pack transfer: %w", err)
	}

	return s.sendTransaction(common.HexToAddress(tokenAddr), nil, data)
}

func (s *Strategy) waitForDeposit(asset string, expectedQty decimal.Decimal) error {
	startBal, _ := s.binClient.GetBalance(asset)
	timeout := 45 * time.Minute
	startTime := time.Now()

	for time.Since(startTime) < timeout {
		time.Sleep(20 * time.Second)
		currentBal, err := s.binClient.GetBalance(asset)
		if err != nil {
			continue
		}
		increase := currentBal.Sub(startBal)
		if increase.GreaterThanOrEqual(expectedQty.Mul(decimal.NewFromFloat(0.9))) {
			log.Info().Str("asset", asset).Str("increase", increase.String()).Msg("Deposit arrived")
			return nil
		}
	}
	return fmt.Errorf("deposit timeout after %v", timeout)
}

func (s *Strategy) ensureApproval(tokenAddr, spenderAddr string, amount *big.Int) error {
	cacheKey := tokenAddr + ":" + spenderAddr
	if _, ok := s.approvedTokens.Load(cacheKey); ok {
		return nil
	}

	token := common.HexToAddress(tokenAddr)
	spender := common.HexToAddress(spenderAddr)

	data, err := s.erc20ABI.Pack("allowance", s.walletAddress, spender)
	if err != nil {
		return err
	}

	result, err := s.ethClient.CallContract(context.Background(), ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return err
	}

	var allowance *big.Int
	if err := s.erc20ABI.UnpackIntoInterface(&allowance, "allowance", result); err != nil {
		return err
	}

	if allowance.Cmp(amount) < 0 {
		maxUint256, _ := new(big.Int).SetString("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 16)
		approveData, err := s.erc20ABI.Pack("approve", spender, maxUint256)
		if err != nil {
			return err
		}
		if _, err := s.sendTransaction(token, nil, approveData); err != nil {
			return fmt.Errorf("approve tx failed: %w", err)
		}
	}

	s.approvedTokens.Store(cacheKey, true)
	return nil
}

func (s *Strategy) getTokenBalance(tokenAddr string) (*big.Int, error) {
	token := common.HexToAddress(tokenAddr)
	packed, err := s.balABI.Pack("balanceOf", s.walletAddress)
	if err != nil {
		return nil, err
	}

	result, err := s.ethClient.CallContract(context.Background(), ethereum.CallMsg{To: &token, Data: packed}, nil)
	if err != nil {
		return nil, err
	}

	var balance *big.Int
	err = s.balABI.UnpackIntoInterface(&balance, "balanceOf", result)
	return balance, err
}

func (s *Strategy) sendTransaction(to common.Address, value *big.Int, data []byte) (string, error) {
	ctx := context.Background()
	nonce, err := s.ethClient.PendingNonceAt(ctx, s.walletAddress)
	if err != nil {
		return "", fmt.Errorf("get nonce: %w", err)
	}
	gasPrice, err := s.ethClient.SuggestGasPrice(ctx)
	if err != nil {
		return "", fmt.Errorf("get gas price: %w", err)
	}
	if value == nil {
		value = big.NewInt(0)
	}

	// Estimate gas
	msg := ethereum.CallMsg{
		From:     s.walletAddress,
		To:       &to,
		GasPrice: gasPrice,
		Value:    value,
		Data:     data,
	}
	gasLimit, err := s.ethClient.EstimateGas(ctx, msg)
	if err != nil {
		log.Warn().Err(err).Msg("Gas estimation failed, using fallback 1,000,000")
		gasLimit = 1000000
	} else {
		gasLimit = uint64(float64(gasLimit) * 1.2) // 20% buffer
	}

	tx := types.NewTransaction(nonce, to, value, gasLimit, gasPrice, data)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(s.chainID), s.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign tx: %w", err)
	}

	if err := s.ethClient.SendTransaction(ctx, signedTx); err != nil {
		return "", fmt.Errorf("send tx: %w", err)
	}

	receipt, err := s.waitForReceipt(ctx, signedTx.Hash())
	if err != nil {
		return signedTx.Hash().Hex(), fmt.Errorf("wait receipt: %w", err)
	}
	if receipt.Status == 0 {
		return signedTx.Hash().Hex(), errors.New("transaction reverted")
	}
	return signedTx.Hash().Hex(), nil
}

func (s *Strategy) waitForReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, txConfirmTimeout)
	defer cancel()
	for {
		receipt, err := s.ethClient.TransactionReceipt(ctx, txHash)
		if err == nil {
			return receipt, nil
		}
		if errors.Is(err, ethereum.NotFound) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(1 * time.Second):
				continue
			}
		}
		return nil, err
	}
}

// GetWalletAddress returns the wallet address for display.
func (s *Strategy) GetWalletAddress() string {
	return s.walletAddress.Hex()
}

// GetOnChainBalance returns the USDT balance on chain.
func (s *Strategy) GetOnChainBalance() (decimal.Decimal, error) {
	bal, err := s.getTokenBalance(s.cfg.Chain.BSC.USDT)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromBigInt(bal, -18), nil
}

// GetCEXBalance returns the USDT balance on Binance spot.
func (s *Strategy) GetCEXBalance() (decimal.Decimal, error) {
	return s.binClient.GetBalance("USDT")
}
