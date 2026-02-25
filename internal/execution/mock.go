package execution

import (
	"context"
	"fmt"
	"time"

	"ocdex/config"
	"ocdex/internal/capital"
	"ocdex/internal/storage"
	"ocdex/pkg/notify"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

type MockExecutor struct {
	notifier   *notify.MultiNotifier
	db         *storage.MySQL
	capitalMgr *capital.RedisCapitalManager
	cfg        config.Config
}

func NewMockExecutor(cfg config.Config, notifier *notify.MultiNotifier, db *storage.MySQL, capitalMgr *capital.RedisCapitalManager) *MockExecutor {
	return &MockExecutor{
		cfg:        cfg,
		notifier:   notifier,
		db:         db,
		capitalMgr: capitalMgr,
	}
}

func (m *MockExecutor) Execute(opp Opportunity) error {
	log.Info().
		Str("币种", opp.Symbol).
		Str("方向", opp.Direction).
		Str("买入价", opp.PriceBuy.StringFixed(4)).
		Str("卖出价", opp.PriceSell.StringFixed(4)).
		Str("差价", opp.Spread.StringFixed(4)).
		Msg("🧪 [模拟执行] 发现机会")

	m.notifier.Send(fmt.Sprintf("🧪 [模拟发现] %s | %s\nBuy: %s | Sell: %s",
		opp.Symbol, opp.Direction, opp.PriceBuy.StringFixed(4), opp.PriceSell.StringFixed(4)))

	// 分发策略
	if m.cfg.Execution.Mode == config.ModeFuturesHedge {
		return m.executeFuturesHedge(opp)
	}

	return nil
}

// executeFuturesHedge Simulates the full Futures Hedging flow
func (m *MockExecutor) executeFuturesHedge(opp Opportunity) error {
	tradeID := fmt.Sprintf("mock-trade-%d", time.Now().UnixNano())

	// 0. Capital Check
	log.Info().Msg("🧪 [模拟] 正在检查 Redis 资金...")
	if err := m.capitalMgr.Reserve(context.Background(), tradeID); err != nil {
		m.notifier.Send("❌ [模拟] 资金不足/已达上限, 放弃执行")
		return fmt.Errorf("mock capital reserve failed: %w", err)
	}
	defer m.capitalMgr.Release(context.Background(), tradeID)
	log.Info().Msg("✅ [模拟] Redis 资金锁定成功")

	// 1. Log Start
	m.logTradeStart(tradeID, opp, "MOCK_HEDGE")
	m.notifier.Send(fmt.Sprintf("⚡ [模拟启动] 对冲套利: %s", opp.Symbol))

	// 2. Simulate Concurrent Execution
	log.Info().Msg("🧪 [模拟] 并发执行: 链上买入 + 合约开空...")
	time.Sleep(1 * time.Second) // Sim delay

	dexTxHash := fmt.Sprintf("0xMOCK_DEX_%d", time.Now().Unix())
	shortOrderID := fmt.Sprintf("MOCK_SHORT_%d", time.Now().Unix())

	m.notifier.Send(fmt.Sprintf("🔒 [模拟锁利] 成功!\nDEX Tx: %s\nShort Order: %s", dexTxHash, shortOrderID))
	m.logTradeStep(tradeID, "DEX_BUY_AND_SHORT", dexTxHash, "", shortOrderID)

	// 3. Simulate Transfer
	log.Info().Msg("🧪 [模拟] 执行充值...")
	time.Sleep(2 * time.Second)
	transferTx := fmt.Sprintf("0xMOCK_TRANSFER_%d", time.Now().Unix())
	m.notifier.Send(fmt.Sprintf("2️⃣ [模拟充值] Tx: %s", transferTx))
	m.logTradeStep(tradeID, "TRANSFERring", transferTx, "", "")

	// 4. Simulate Wait
	log.Info().Msg("🧪 [模拟] 等待到账 (加速模式 5s)...")
	time.Sleep(5 * time.Second)
	m.notifier.Send("3️⃣ [模拟到账] CEX 资金已更新")
	m.logTradeStep(tradeID, "DEPOSIT_ARRIVED", "", "", "")

	// 5. Simulate Sell & Close
	log.Info().Msg("🧪 [模拟] 现货卖出 & 平空...")
	time.Sleep(1 * time.Second)

	spotOrderID := fmt.Sprintf("MOCK_SPOT_%d", time.Now().Unix())
	closeOrderID := fmt.Sprintf("MOCK_CLOSE_%d", time.Now().Unix())

	m.notifier.Send(fmt.Sprintf("🎉 [模拟完成] 对冲闭环!\nSpot Sell: %s\nClose Short: %s", spotOrderID, closeOrderID))

	// Final Log
	// We call a simplified complete logger or just leave it as is
	return nil
}

// === Logging Helpers (Reused from RealExecutor logic roughly) ===

func (m *MockExecutor) logTradeStart(tradeID string, opp Opportunity, mode string) {
	if m.db == nil {
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
		Quantity:      quantity,
		DetectedAt:    time.Now(),
		Status:        "MOCK_STARTED",
	}
	m.db.SaveTradeHistory(history)
}

func (m *MockExecutor) logTradeStep(tradeID string, status string, txHash string, spotOrderID string, shortOrderID string) {
	if m.db == nil {
		return
	}
	// In a real mock, we might update the record status to "MOCK_STEP_X"
}

// Interactive Methods (Stubbed)
func (m *MockExecutor) BuyOnDEX(opp Opportunity) (string, error)      { return "MOCK", nil }
func (m *MockExecutor) TransferToCEX(opp Opportunity) (string, error) { return "MOCK", nil }
func (m *MockExecutor) SellOnCEX(opp Opportunity) (int64, error)      { return 0, nil }
