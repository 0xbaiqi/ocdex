package newcoin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// AutoTrader subscribes to Monitor price updates and automatically executes
// profitable arbitrage trades when AutoTrade is enabled.
//
// Trading Logic:
//
//	Initial state: CEX has N tokens, DEX has N tokens, short covers 2N
//
//	Direction A (CEX expensive, DEX cheap):
//	  1. CEX sell + DEX buy (simultaneous) → lock in price spread as USDT profit
//	  2. Transfer tokens from DEX → CEX (rebalance)
//	  3. Wait for deposit → ready for next round
//
//	Direction B (DEX expensive, CEX cheap):
//	  1. CEX buy + DEX sell (simultaneous) → lock in price spread as USDT profit
//	  2. Need to withdraw from CEX → DEX (rebalance)
//	  ⚠️ Withdrawal API not implemented → cannot automate this direction
type AutoTrader struct {
	strategy *Strategy
	monitor  *Monitor
	state    *StateManager

	mu        sync.Mutex
	running   bool
	cancel    context.CancelFunc
	cooldown  time.Duration
	lastExec  time.Time
	executing sync.Mutex // prevents concurrent trade execution
}

// NewAutoTrader creates a new auto trader instance.
func NewAutoTrader(strategy *Strategy, monitor *Monitor, state *StateManager) *AutoTrader {
	return &AutoTrader{
		strategy: strategy,
		monitor:  monitor,
		state:    state,
		cooldown: 30 * time.Second, // minimum interval between trades
	}
}

// Start begins the auto trading loop. It subscribes to monitor price updates
// and evaluates each price change for arbitrage opportunities.
func (at *AutoTrader) Start(ctx context.Context) {
	at.mu.Lock()
	if at.running {
		at.mu.Unlock()
		return
	}
	at.running = true
	ctx, at.cancel = context.WithCancel(ctx)
	at.mu.Unlock()

	ch := at.monitor.Subscribe()
	defer at.monitor.Unsubscribe(ch)

	log.Info().Msg("AutoTrader started — listening for price changes")

	for {
		select {
		case <-ctx.Done():
			at.mu.Lock()
			at.running = false
			at.mu.Unlock()
			log.Info().Msg("AutoTrader stopped")
			return
		case snap, ok := <-ch:
			if !ok {
				at.mu.Lock()
				at.running = false
				at.mu.Unlock()
				log.Info().Msg("AutoTrader: price channel closed")
				return
			}
			at.evaluate(snap)
		}
	}
}

// Stop stops the auto trading loop.
func (at *AutoTrader) Stop() {
	at.mu.Lock()
	defer at.mu.Unlock()
	if at.cancel != nil {
		at.cancel()
	}
	at.running = false
}

// IsRunning returns whether the auto trader is active.
func (at *AutoTrader) IsRunning() bool {
	at.mu.Lock()
	defer at.mu.Unlock()
	return at.running
}

// evaluate checks a price snapshot for a profitable opportunity and executes if conditions are met.
func (at *AutoTrader) evaluate(snap PriceSnapshot) {
	// 1. Check if auto trade is enabled
	settings := at.state.GetState().Settings
	if !settings.AutoTrade {
		return
	}

	// 2. Check if hedge is active (must have positions before trading)
	st := at.state.GetState()
	if st.Hedge == nil || !st.Hedge.Active {
		return
	}

	tc := st.Token
	if tc == nil {
		return
	}

	// 3. Both prices must be available
	if !snap.CEXPrice.IsPositive() || !snap.DEXPrice.IsPositive() {
		return
	}

	tradeAmountUSD := settings.TradeAmountUSD
	if tradeAmountUSD <= 0 {
		tradeAmountUSD = 10
	}

	minSpreadPct := settings.MinSpreadPct
	if minSpreadPct <= 0 {
		minSpreadPct = 0.5
	}

	// Estimate costs
	est := at.strategy.EstimateTradeCost(tradeAmountUSD)

	// ═══════════════════════════════════════════════
	// Direction A: CEX expensive, DEX cheap
	// Action: CEX sell + DEX buy → transfer to CEX to rebalance
	// ═══════════════════════════════════════════════
	// Spread = (CEX - DEX) / DEX × 100 (positive = CEX is more expensive)
	spreadA := snap.CEXPrice.Sub(snap.DEXPrice).Div(snap.DEXPrice).Mul(decimal.NewFromInt(100))

	if spreadA.GreaterThan(decimal.NewFromFloat(minSpreadPct)) {
		grossProfit := spreadA.Div(decimal.NewFromInt(100)).Mul(decimal.NewFromFloat(tradeAmountUSD))
		netProfit := grossProfit.Sub(decimal.NewFromFloat(est.DirA_Total))

		if netProfit.IsPositive() {
			log.Info().
				Str("symbol", tc.Symbol).
				Str("CEX", snap.CEXPrice.StringFixed(6)).
				Str("DEX", snap.DEXPrice.StringFixed(6)).
				Str("spread", spreadA.StringFixed(3)+"%").
				Str("est_profit", netProfit.StringFixed(4)).
				Float64("amount", tradeAmountUSD).
				Msg("AutoTrader: profitable opportunity detected (CEX sell + DEX buy)")

			go at.executeDirA(tradeAmountUSD, snap, tc)
		}
		return
	}

	// ═══════════════════════════════════════════════
	// Direction B: DEX expensive, CEX cheap
	// Action: CEX buy + DEX sell → need withdrawal (NOT IMPLEMENTED)
	// ═══════════════════════════════════════════════
	spreadB := snap.DEXPrice.Sub(snap.CEXPrice).Div(snap.CEXPrice).Mul(decimal.NewFromInt(100))
	if spreadB.GreaterThan(decimal.NewFromFloat(minSpreadPct)) {
		grossProfit := spreadB.Div(decimal.NewFromInt(100)).Mul(decimal.NewFromFloat(tradeAmountUSD))
		netProfit := grossProfit.Sub(decimal.NewFromFloat(est.DirB_Total))

		if netProfit.IsPositive() {
			log.Debug().
				Str("symbol", tc.Symbol).
				Str("CEX", snap.CEXPrice.StringFixed(6)).
				Str("DEX", snap.DEXPrice.StringFixed(6)).
				Str("spread", spreadB.StringFixed(3)+"%").
				Str("est_profit", netProfit.StringFixed(4)).
				Msg("AutoTrader: Direction B opportunity (CEX buy + DEX sell) — skipping, withdrawal not implemented")
		}
	}
}

// executeDirA executes Direction A: simultaneous CEX sell + DEX buy, then rebalance.
func (at *AutoTrader) executeDirA(amountUSD float64, snap PriceSnapshot, tc *TokenConfig) {
	// Execution lock: only one trade at a time
	if !at.executing.TryLock() {
		log.Debug().Msg("AutoTrader: execution in progress, skipping")
		return
	}
	defer at.executing.Unlock()

	// Cooldown check
	at.mu.Lock()
	if time.Since(at.lastExec) < at.cooldown {
		at.mu.Unlock()
		log.Debug().
			Dur("cooldown_remaining", at.cooldown-time.Since(at.lastExec)).
			Msg("AutoTrader: in cooldown, skipping")
		return
	}
	at.lastExec = time.Now()
	at.mu.Unlock()

	// Re-check auto trade enabled (could have been turned off while waiting)
	if !at.state.GetState().Settings.AutoTrade {
		return
	}

	// Re-verify opportunity with aggregator quote (real executable price)
	latestSnap := at.monitor.GetLatest()
	if !latestSnap.CEXPrice.IsPositive() || !latestSnap.DEXPrice.IsPositive() {
		return
	}

	settings := at.state.GetState().Settings

	// Use aggregator for accurate DEX price (accounts for slippage across all pools)
	var realDEXPrice decimal.Decimal
	if at.strategy.aggregator != nil {
		aggQuote, err := at.strategy.aggregator.GetBestQuote(
			tc.ContractAddress, tc.Decimals, amountUSD, true,
		)
		if err == nil && aggQuote.Price.IsPositive() {
			realDEXPrice = aggQuote.Price
			log.Debug().
				Str("source", aggQuote.Source).
				Str("pool_dex", latestSnap.DEXPrice.StringFixed(8)).
				Str("agg_dex", realDEXPrice.StringFixed(8)).
				Msg("AutoTrader: using aggregator price for verification")
		}
	}
	if !realDEXPrice.IsPositive() {
		realDEXPrice = latestSnap.DEXPrice // fallback to pool price
	}

	latestSpread := latestSnap.CEXPrice.Sub(realDEXPrice).Div(realDEXPrice).Mul(decimal.NewFromInt(100))

	est := at.strategy.EstimateTradeCost(amountUSD)
	grossProfit := latestSpread.Div(decimal.NewFromInt(100)).Mul(decimal.NewFromFloat(amountUSD))
	netProfit := grossProfit.Sub(decimal.NewFromFloat(est.DirA_Total))

	if !netProfit.IsPositive() || latestSpread.LessThanOrEqual(decimal.NewFromFloat(settings.MinSpreadPct)) {
		log.Debug().
			Str("spread", latestSpread.StringFixed(3)+"%").
			Str("net_profit", netProfit.StringFixed(4)).
			Str("cex", latestSnap.CEXPrice.StringFixed(8)).
			Str("dex_agg", realDEXPrice.StringFixed(8)).
			Msg("AutoTrader: opportunity disappeared on re-check (aggregator price)")
		return
	}

	log.Info().
		Str("symbol", tc.Symbol).
		Str("spread", latestSpread.StringFixed(3)+"%").
		Str("net_profit", netProfit.StringFixed(4)).
		Float64("amount", amountUSD).
		Msg("AutoTrader: EXECUTING sell CEX + buy DEX")

	// Execute arbitrage: CEX sell + DEX buy simultaneously, then rebalance
	trade, err := at.strategy.ArbitrageSellCEXBuyDEX(amountUSD)
	if err != nil {
		log.Error().Err(err).
			Str("symbol", tc.Symbol).
			Msg("AutoTrader: trade execution failed")

		if trade != nil {
			at.state.UpdateTrade(trade.ID, func(t *Trade) {
				if t.Error != "" {
					t.Error = fmt.Sprintf("[auto] %s", t.Error)
				} else {
					t.Error = fmt.Sprintf("[auto] %s", err.Error())
				}
			})
		}
		return
	}

	log.Info().
		Str("symbol", tc.Symbol).
		Str("trade_id", trade.ID).
		Float64("profit", trade.ProfitUSD).
		Msg("AutoTrader: trade completed successfully")
}
