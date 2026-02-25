package execution

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
	"ocdex/internal/capital"
	"ocdex/internal/storage"
	"ocdex/pkg/notify"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

const (
	TxConfirmTimeout = 60 * time.Second
)

const RouterABI = `[
	{"inputs":[{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint256","name":"amountOutMin","type":"uint256"},{"internalType":"address[]","name":"path","type":"address[]"},{"internalType":"address","name":"to","type":"address"},{"internalType":"uint256","name":"deadline","type":"uint256"}],"name":"swapExactTokensForTokens","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],"stateMutability":"nonpayable","type":"function"}
]`

const ERC20ABI = `[
	{"inputs":[{"internalType":"address","name":"spender","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"approve","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"},
	{"inputs":[{"internalType":"address","name":"account","type":"address"},{"internalType":"address","name":"spender","type":"address"}],"name":"allowance","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
	{"inputs":[],"name":"decimals","outputs":[{"internalType":"uint8","name":"","type":"uint8"}],"stateMutability":"view","type":"function"},
	{"inputs":[{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"transfer","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"}
]`

type RealExecutor struct {
	notifier         *notify.MultiNotifier
	cfg              config.Config
	db               *storage.MySQL
	ethClient        *ethclient.Client
	privateKey       *ecdsa.PrivateKey
	walletAddress    common.Address
	binanceClient    *cex.BinanceClient
	binFuturesClient *cex.BinanceFuturesClient
	capitalMgr       *capital.RedisCapitalManager
	chainID          *big.Int
	routerABI        abi.ABI
	erc20ABI         abi.ABI
	approvedTokens   sync.Map
}

func NewRealExecutor(
	cfg config.Config,
	notifier *notify.MultiNotifier,
	db *storage.MySQL,
	ethClient *ethclient.Client,
	binClient *cex.BinanceClient,
	binFuturesClient *cex.BinanceFuturesClient,
	capitalMgr *capital.RedisCapitalManager,
	privateKey *ecdsa.PrivateKey,
	walletAddress common.Address,
) (*RealExecutor, error) {
	chainID, err := ethClient.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get chain id: %w", err)
	}

	rABI, err := abi.JSON(strings.NewReader(RouterABI))
	if err != nil {
		return nil, fmt.Errorf("failed to parse router abi: %w", err)
	}
	eABI, err := abi.JSON(strings.NewReader(ERC20ABI))
	if err != nil {
		return nil, fmt.Errorf("failed to parse erc20 abi: %w", err)
	}

	exec := &RealExecutor{
		notifier:         notifier,
		cfg:              cfg,
		db:               db,
		ethClient:        ethClient,
		privateKey:       privateKey,
		walletAddress:    walletAddress,
		binanceClient:    binClient,
		binFuturesClient: binFuturesClient,
		capitalMgr:       capitalMgr,
		chainID:          chainID,
		routerABI:        rABI,
		erc20ABI:         eABI,
	}

	usdtAddr := cfg.Chain.BSC.USDT
	routerAddr := cfg.Chain.BSC.RouterV2
	log.Info().Str("资产", "USDT").Msg("🔍 启动时检查基础资产授权状态...")

	usdtDecimals, err := exec.getTokenDecimals(usdtAddr)
	if err == nil {
		amount := decimal.NewFromFloat(1000).Mul(decimal.New(1, int32(usdtDecimals))).BigInt()
		if err := exec.ensureApproval(usdtAddr, routerAddr, amount); err == nil {
			log.Info().Msg("✅ 基础资产 USDT 授权状态就绪")
		} else {
			log.Warn().Err(err).Msg("⚠️ 启动时 USDT 授权检查失败，将在执行时重试")
		}
	}

	return exec, nil
}

func (e *RealExecutor) Execute(opp Opportunity) error {
	log.Info().
		Str("币种", opp.Symbol).
		Str("方向", opp.Direction).
		Str("模式", string(e.cfg.Execution.Mode)).
		Msg("🚀 开始套利执行")

	if e.cfg.Execution.Mode == config.ModeDepositSell {
		return e.executeDepositSync(opp)
	}

	if e.cfg.Execution.Mode == config.ModeFuturesHedge {
		return e.executeFuturesHedge(opp)
	}

	if opp.Direction != "BUY_DEX_SELL_CEX" {
		return fmt.Errorf("暂不支持方向: %s", opp.Direction)
	}

	// Legacy Default Execution (if needed, or just redirect to DepositSync)
	return e.executeDepositSync(opp)
}

// executeFuturesHedge logic
func (e *RealExecutor) executeFuturesHedge(opp Opportunity) error {
	tradeID := fmt.Sprintf("trade-%d", time.Now().UnixNano())
	if err := e.capitalMgr.Reserve(context.Background(), tradeID); err != nil {
		return fmt.Errorf("capital reserve failed: %w", err)
	}
	defer e.capitalMgr.Release(context.Background(), tradeID)

	e.notifier.Send(fmt.Sprintf("⚡ 启动「合约对冲」套利: %s", opp.Symbol))
	e.logTradeStart(tradeID, opp, string(config.ModeFuturesHedge))

	// 使用 CEXSymbol (已含USDT后缀, 如 "1MBABYDOGEUSDT")
	futuresSymbol := opp.CEXSymbol
	if futuresSymbol == "" {
		futuresSymbol = opp.Symbol + "USDT"
	}
	if !e.binFuturesClient.HasFuturesContract(futuresSymbol) {
		e.notifier.Send(fmt.Sprintf("⚠️ %s 无合约，降级为普通充值套利", futuresSymbol))
		return e.executeDepositSync(opp)
	}

	// 计算 CEX 下单数量: 实际代币数 ÷ CEX倍率
	// 例: 250亿 BABYDOGE ÷ 1000000 = 25万 (1MBABYDOGE 单位)
	rawQuantity := decimal.NewFromBigInt(opp.Amount, int32(-opp.Decimals))
	cexMultiplier := decimal.NewFromInt(opp.CEXMultiplier)
	if cexMultiplier.LessThanOrEqual(decimal.Zero) {
		cexMultiplier = decimal.NewFromInt(1)
	}
	quantity := rawQuantity.Div(cexMultiplier)

	log.Info().
		Str("symbol", futuresSymbol).
		Str("原始数量", rawQuantity.StringFixed(4)).
		Int64("倍率", opp.CEXMultiplier).
		Str("下单数量", quantity.StringFixed(4)).
		Msg("合约开空参数")

	var wg sync.WaitGroup
	wg.Add(2)

	var buyTxHash string
	var buyErr error
	var shortOrderID string
	var shortAvgPrice decimal.Decimal
	var shortErr error

	// A: DEX Buy
	go func() {
		defer wg.Done()
		buyTxHash, buyErr = e.BuyOnDEX(opp)
	}()

	// B: CEX Short (1x Hedge)
	go func() {
		defer wg.Done()
		shortOrderID, shortAvgPrice, shortErr = e.binFuturesClient.OpenShort(futuresSymbol, quantity)
	}()

	wg.Wait()

	e.logTradeStep(tradeID, "DEX_BUY_AND_SHORT", "", buyTxHash, shortOrderID)

	if buyErr != nil && shortErr != nil {
		e.logTradeError(tradeID, fmt.Sprintf("Double Fail: %v, %v", buyErr, shortErr))
		return fmt.Errorf("DOUBLE FAIL: BuyErr=%v, ShortErr=%v", buyErr, shortErr)
	}
	if buyErr != nil && shortErr == nil {
		e.notifier.Send("🚨 买入失败但做空成功，正在平空止损...")
		e.binFuturesClient.CloseShort(futuresSymbol, quantity)
		e.logTradeError(tradeID, "Buy Failed, Short Closed")
		return fmt.Errorf("buy failed, short closed: %w", buyErr)
	}
	if buyErr == nil && shortErr != nil {
		e.notifier.Send(fmt.Sprintf("⚠️ 做空失败 (%v)，转为充值套利流程", shortErr))
		e.logTradeError(tradeID, "Short Failed, Degraded to DepositSell")
		return e.executeDepositSync_TransferSell(opp, buyTxHash)
	}

	e.notifier.Send(fmt.Sprintf("🔒 成功锁利!\nDEX: %s\nShort: %s @ %s", buyTxHash, shortOrderID, shortAvgPrice))

	depTxHash, err := e.TransferToCEX(opp)
	if err != nil {
		e.notifier.Send(fmt.Sprintf("🚨 充值失败! %v", err))
		e.logTradeError(tradeID, "Transfer Failed: "+err.Error())
		return err
	}
	e.notifier.Send(fmt.Sprintf("2️⃣ 充值中: %s", depTxHash))
	e.logTradeStep(tradeID, "TRANSFERring", depTxHash, "", "")

	if err := e.waitForDeposit(opp); err != nil {
		e.notifier.Send(fmt.Sprintf("🚨 等待超时! %v", err))
		e.logTradeError(tradeID, "Wait Deposit Timeout")
		return err
	}
	e.logTradeStep(tradeID, "DEPOSIT_ARRIVED", "", "", "")

	spotOrderID, err := e.SellOnCEX(opp)
	if err != nil {
		e.notifier.Send(fmt.Sprintf("❌ 现货卖出失败: %v", err))
		e.logTradeError(tradeID, "Spot Sell Failed")
		return err
	}

	closeID, _, err := e.binFuturesClient.CloseShort(futuresSymbol, quantity)
	if err != nil {
		e.notifier.Send(fmt.Sprintf("❌ 平空失败! 请手动处理! %v", err))
		e.logTradeError(tradeID, "Short Close Failed")
		return err
	}

	e.notifier.Send(fmt.Sprintf("🎉 对冲套利闭环完成!\nSpot: %d\nClose: %s", spotOrderID, closeID))
	e.logTradeComplete(tradeID, spotOrderID, closeID)
	return nil
}

// executeDepositSync implements the fully synchronous 'Deposit & Sell' workflow
func (e *RealExecutor) executeDepositSync(opp Opportunity) error {
	tradeID := fmt.Sprintf("trade-%d", time.Now().UnixNano())
	e.logTradeStart(tradeID, opp, string(config.ModeDepositSell))

	e.notifier.Send(fmt.Sprintf("🏦 启动「充值套利」流程: %s", opp.Symbol))

	txHash, err := e.BuyOnDEX(opp)
	if err != nil {
		e.logTradeError(tradeID, "DEX Buy Failed: "+err.Error())
		return err
	}
	e.notifier.Send(fmt.Sprintf("1️⃣ 链上买入成功: %s", txHash))

	return e.executeDepositSync_TransferSell(opp, txHash)
}

// executeDepositSync_TransferSell continues from Transfer step
func (e *RealExecutor) executeDepositSync_TransferSell(opp Opportunity, buyTxHash string) error {
	depTxHash, err := e.TransferToCEX(opp)
	if err != nil {
		return err
	}
	e.notifier.Send(fmt.Sprintf("2️⃣ 正在充值中: %s\n等待CEX到账...", depTxHash))

	if err := e.waitForDeposit(opp); err != nil {
		return err
	}

	orderID, err := e.SellOnCEX(opp)
	if err != nil {
		return err
	}
	e.notifier.Send(fmt.Sprintf("4️⃣ CEX卖出成功! 订单号: %d\n🎉 流程结束!", orderID))
	return nil
}

func (e *RealExecutor) waitForDeposit(opp Opportunity) error {
	targetAsset := opp.Symbol
	startBal, _ := e.binanceClient.GetBalance(targetAsset)
	startTime := time.Now()
	timeout := 45 * time.Minute
	// CEX 余额是以 CEX 单位显示的, 需要除以 CEXMultiplier
	rawAmount := decimal.NewFromBigInt(opp.Amount, int32(-opp.Decimals))
	cexMultiplier := decimal.NewFromInt(opp.CEXMultiplier)
	if cexMultiplier.LessThanOrEqual(decimal.Zero) {
		cexMultiplier = decimal.NewFromInt(1)
	}
	expectedAmount := rawAmount.Div(cexMultiplier)

	log.Debug().Str("Asset", targetAsset).Str("StartBal", startBal.String()).Msg("Start waiting for deposit")

	for time.Since(startTime) < timeout {
		time.Sleep(20 * time.Second)
		currentBal, err := e.binanceClient.GetBalance(targetAsset)
		if err != nil {
			log.Warn().Err(err).Msg("Poll balance failed")
			continue
		}
		increase := currentBal.Sub(startBal)
		if increase.GreaterThanOrEqual(expectedAmount.Mul(decimal.NewFromFloat(0.9))) {
			log.Info().Msg("✅ 资金已到账!")
			e.notifier.Send(fmt.Sprintf("3️⃣ CEX资金到账! 数量: %s", increase.StringFixed(4)))
			return nil
		}
	}
	return fmt.Errorf("timeout")
}

// === Logging Helpers ===

func (e *RealExecutor) logTradeStart(tradeID string, opp Opportunity, mode string) {
	if e.db == nil {
		return
	}

	quantity := decimal.NewFromBigInt(opp.Amount, int32(-opp.Decimals))

	history := &storage.TradeHistory{
		TradeID:       tradeID,
		Symbol:        opp.Symbol,
		Direction:     opp.Direction,
		Mode:          mode,
		DEXPrice:      opp.PriceBuy,
		CEXPrice:      opp.PriceSell,
		SpreadPercent: opp.Spread,
		AmountUSD:     decimal.NewFromInt(0),
		Quantity:      quantity,
		DetectedAt:    time.Now(),
		Status:        "STARTED",
	}
	e.db.SaveTradeHistory(history)
}

func (e *RealExecutor) logTradeStep(tradeID string, status string, txHash string, spotOrderID string, shortOrderID string) {
	if e.db == nil {
		return
	}
	// Implementation note: update logic
}

func (e *RealExecutor) logTradeError(tradeID string, msg string) {
	if e.db == nil {
		return
	}
}

func (e *RealExecutor) logTradeComplete(tradeID string, spotID int64, closeID string) {
	if e.db == nil {
		return
	}
}

// === Public Methods for Web UI / Manual Control ===

func (e *RealExecutor) BuyOnDEX(opp Opportunity) (string, error) {
	return e.swapOnDEX(opp)
}

func (e *RealExecutor) TransferToCEX(opp Opportunity) (string, error) {
	if e.cfg.Execution.Dep.Address == "" {
		return "", errors.New("CEX deposit address not configured")
	}

	tokenAddr := opp.TokenAddress
	balance, err := e.getTokenBalance(tokenAddr)
	if err != nil {
		return "", fmt.Errorf("failed to get token balance: %w", err)
	}

	if balance.Cmp(big.NewInt(0)) <= 0 {
		return "", errors.New("insufficient token balance to transfer")
	}

	log.Info().Str("Token", opp.Symbol).Str("Balance", balance.String()).Str("To", e.cfg.Execution.Dep.Address).Msg("📤 正在充值到 CEX...")

	toAddr := common.HexToAddress(e.cfg.Execution.Dep.Address)
	data, err := e.erc20ABI.Pack("transfer", toAddr, balance)
	if err != nil {
		return "", fmt.Errorf("failed to pack transfer data: %w", err)
	}

	return e.sendTransaction(common.HexToAddress(tokenAddr), nil, data)
}

func (e *RealExecutor) SellOnCEX(opp Opportunity) (int64, error) {
	return e.sellOnCEX(opp)
}

func (e *RealExecutor) ensureApproval(tokenAddr, spenderAddr string, amount *big.Int) error {
	cacheKey := tokenAddr + ":" + spenderAddr
	if _, approved := e.approvedTokens.Load(cacheKey); approved {
		return nil
	}

	ctx := context.Background()
	token := common.HexToAddress(tokenAddr)
	spender := common.HexToAddress(spenderAddr)

	data, err := e.erc20ABI.Pack("allowance", e.walletAddress, spender)
	if err != nil {
		return fmt.Errorf("failed to pack allowance call: %w", err)
	}

	callMsg := ethereum.CallMsg{
		To:   &token,
		Data: data,
	}
	result, err := e.ethClient.CallContract(ctx, callMsg, nil)
	if err != nil {
		return fmt.Errorf("failed to call allowance: %w", err)
	}

	var allowance *big.Int
	err = e.erc20ABI.UnpackIntoInterface(&allowance, "allowance", result)
	if err != nil {
		return fmt.Errorf("failed to unpack allowance: %w", err)
	}

	if allowance.Cmp(amount) < 0 {
		log.Info().
			Str("token", tokenAddr).
			Str("required", amount.String()).
			Str("current", allowance.String()).
			Msg("📢 授权额度不足，正在进行无限授权...")

		maxUint256, _ := new(big.Int).SetString("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 16)
		approveData, err := e.erc20ABI.Pack("approve", spender, maxUint256)
		if err != nil {
			return fmt.Errorf("failed to pack approve call: %w", err)
		}

		txHash, err := e.sendTransaction(token, nil, approveData)
		if err != nil {
			return fmt.Errorf("failed to send approve transaction: %w", err)
		}
		log.Info().Str("TxHash", txHash).Msg("✅ 授权交易已成功确认")
	}

	e.approvedTokens.Store(cacheKey, true)
	return nil
}

func (e *RealExecutor) getTokenDecimals(tokenAddr string) (uint8, error) {
	token := common.HexToAddress(tokenAddr)
	data, err := e.erc20ABI.Pack("decimals")
	if err != nil {
		return 0, err
	}

	result, err := e.ethClient.CallContract(context.Background(), ethereum.CallMsg{
		To:   &token,
		Data: data,
	}, nil)
	if err != nil {
		return 0, err
	}

	var decimals uint8
	err = e.erc20ABI.UnpackIntoInterface(&decimals, "decimals", result)
	return decimals, err
}

func (e *RealExecutor) swapOnDEX(opp Opportunity) (string, error) {
	usdtDecimals, err := e.getTokenDecimals(e.cfg.Chain.BSC.USDT)
	if err != nil {
		return "", err
	}
	// 使用动态交易金额 (由 ArbitrageDetector 根据池子深度计算)
	tradeAmountUSD := opp.TradeAmountUSD
	if tradeAmountUSD.IsZero() {
		// fallback: 如果没设置动态金额，退回到配置值
		tradeAmountUSD = decimal.NewFromFloat(e.cfg.Strategy.TradeAmountUSD)
	}
	amountIn := tradeAmountUSD.Mul(decimal.New(1, int32(usdtDecimals))).BigInt()

	// 使用动态滑点: 由 ArbitrageDetector 根据利润率计算
	slippage := opp.MaxSlippage
	if slippage.IsZero() {
		// fallback: 如果没设置动态滑点，用配置值
		slippage = decimal.NewFromFloat(e.cfg.Strategy.Slippage / 100)
	}
	expectedOut := decimal.NewFromBigInt(opp.Amount, 0)
	amountOutMin := expectedOut.Mul(decimal.NewFromInt(1).Sub(slippage)).BigInt()

	log.Info().
		Str("token", opp.Symbol).
		Str("slippage", slippage.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
		Str("expectedOut", decimal.NewFromBigInt(opp.Amount, int32(-opp.Decimals)).StringFixed(4)).
		Str("amountOutMin", decimal.NewFromBigInt(amountOutMin, int32(-opp.Decimals)).StringFixed(4)).
		Msg("DEX Swap 滑点保护")

	path := []common.Address{
		common.HexToAddress(e.cfg.Chain.BSC.USDT),
		common.HexToAddress(opp.TokenAddress),
	}
	deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())

	data, err := e.routerABI.Pack("swapExactTokensForTokens", amountIn, amountOutMin, path, e.walletAddress, deadline)
	if err != nil {
		return "", err
	}

	return e.sendTransaction(common.HexToAddress(e.cfg.Chain.BSC.RouterV2), nil, data)
}

func (e *RealExecutor) sendTransaction(to common.Address, value *big.Int, data []byte) (string, error) {
	ctx := context.Background()
	nonce, err := e.ethClient.PendingNonceAt(ctx, e.walletAddress)
	if err != nil {
		return "", fmt.Errorf("failed to get nonce: %w", err)
	}
	gasPrice, err := e.ethClient.SuggestGasPrice(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get gas price: %w", err)
	}

	if value == nil {
		value = big.NewInt(0)
	}

	tx := types.NewTransaction(nonce, to, value, 300000, gasPrice, data)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(e.chainID), e.privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign tx: %w", err)
	}

	if err := e.ethClient.SendTransaction(ctx, signedTx); err != nil {
		return "", fmt.Errorf("failed to send tx: %w", err)
	}

	receipt, err := e.waitForReceipt(ctx, signedTx.Hash())
	if err != nil {
		return signedTx.Hash().Hex(), fmt.Errorf("failed to wait for receipt: %w", err)
	}
	if receipt.Status == 0 {
		return signedTx.Hash().Hex(), errors.New("transaction reverted")
	}

	return signedTx.Hash().Hex(), nil
}

func (e *RealExecutor) waitForReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, TxConfirmTimeout)
	defer cancel()
	for {
		receipt, err := e.ethClient.TransactionReceipt(ctx, txHash)
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

func (e *RealExecutor) sellOnCEX(opp Opportunity) (int64, error) {
	// 使用 CEXSymbol, 并除以 CEXMultiplier 得到 CEX 单位数量
	rawQuantity := decimal.NewFromBigInt(opp.Amount, int32(-opp.Decimals))
	cexMultiplier := decimal.NewFromInt(opp.CEXMultiplier)
	if cexMultiplier.LessThanOrEqual(decimal.Zero) {
		cexMultiplier = decimal.NewFromInt(1)
	}
	quantity := rawQuantity.Div(cexMultiplier).StringFixed(4)

	symbol := opp.CEXSymbol
	if symbol == "" {
		symbol = opp.Symbol + "USDT"
	}

	log.Info().
		Str("symbol", symbol).
		Str("数量", quantity).
		Msg("CEX 现货卖出")

	return e.binanceClient.PlaceMarketSellOrder(symbol, quantity)
}

func (e *RealExecutor) getTokenBalance(tokenAddr string) (*big.Int, error) {
	token := common.HexToAddress(tokenAddr)
	data, err := abi.JSON(strings.NewReader(`[{"constant":true,"inputs":[{"name":"_owner","type":"address"}],"name":"balanceOf","outputs":[{"name":"balance","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"}]`))
	if err != nil {
		return nil, err
	}

	packed, err := data.Pack("balanceOf", e.walletAddress)
	if err != nil {
		return nil, err
	}

	result, err := e.ethClient.CallContract(context.Background(), ethereum.CallMsg{To: &token, Data: packed}, nil)
	if err != nil {
		return nil, err
	}

	var balance *big.Int
	err = data.UnpackIntoInterface(&balance, "balanceOf", result)
	return balance, err
}
