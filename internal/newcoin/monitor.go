package newcoin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"ocdex/config"
	"ocdex/internal/cexstream"
	"ocdex/internal/engine"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// PriceSnapshot holds the latest price comparison data.
type PriceSnapshot struct {
	CEXPrice      decimal.Decimal `json:"cex_price"`
	DEXPrice      decimal.Decimal `json:"dex_price"`
	PoolDEXPrice  decimal.Decimal `json:"pool_dex_price"` // raw pool price (for display)
	AggDEXPrice   decimal.Decimal `json:"agg_dex_price"`  // aggregator price (more accurate)
	SpreadPct     decimal.Decimal `json:"spread_pct"`     // (CEX - DEX) / CEX * 100
	PoolLiquidity decimal.Decimal `json:"pool_liquidity"`
	Timestamp     time.Time       `json:"timestamp"`
}

// Monitor wraps CEXStream + LogWatcher + PoolManager for a single coin.
type Monitor struct {
	cfg         *config.Config
	binAPI      *BinanceAPI
	aggregator  *DEXAggregator
	cexStream   *cexstream.Stream
	poolManager *engine.PoolManager
	logWatcher  *engine.LogWatcher

	mu          sync.RWMutex
	latest      PriceSnapshot
	token       *TokenConfig
	subscribers []chan PriceSnapshot
	running     bool
	cancel      context.CancelFunc
	aggDEXPrice decimal.Decimal // latest aggregator DEX price
}

// NewMonitor creates a new price monitor.
func NewMonitor(cfg *config.Config, binAPI *BinanceAPI, aggregator *DEXAggregator) *Monitor {
	taxDetector := engine.NewTaxDetector(nil, cfg.Chain.BSC) // nil ethClient OK for tax detector init
	pm := engine.NewPoolManager(taxDetector)
	stream := cexstream.NewStream()

	return &Monitor{
		cfg:         cfg,
		binAPI:      binAPI,
		aggregator:  aggregator,
		cexStream:   stream,
		poolManager: pm,
	}
}

// Configure sets up the monitor for a specific token.
// Registers all discovered pools in PoolManager. If WBNB pools exist, also registers WBNB/USDT pool.
func (m *Monitor) Configure(tc *TokenConfig) error {
	m.mu.Lock()
	m.token = tc
	m.mu.Unlock()

	// Register all discovered pools
	hasWBNB := false
	for _, pool := range tc.Pools {
		if pool.Version == 3 {
			m.poolManager.AddPoolV3(
				pool.Address, pool.Token0, pool.Token1,
				tc.Symbol, tc.ContractAddress, tc.Decimals, pool.FeeTier,
			)
		} else {
			m.poolManager.AddPool(
				pool.Address, pool.Token0, pool.Token1,
				tc.Symbol, tc.ContractAddress, tc.Decimals,
			)
		}
		if pool.QuoteToken == "WBNB" {
			hasWBNB = true
		}
	}

	// If WBNB pools exist, register WBNB/USDT pool for price conversion
	if hasWBNB && m.binAPI != nil {
		ctx := context.Background()
		wbnbPools := m.binAPI.DiscoverWBNBUSDTPools(ctx)
		for _, wp := range wbnbPools {
			if wp.Version == 3 {
				m.poolManager.AddPoolV3(wp.Address, wp.Token0, wp.Token1, "WBNB", m.cfg.Chain.BSC.WBNB, 18, wp.FeeTier)
			} else {
				m.poolManager.AddPool(wp.Address, wp.Token0, wp.Token1, "WBNB", m.cfg.Chain.BSC.WBNB, 18)
			}
		}
		log.Info().Int("wbnb_usdt_pools", len(wbnbPools)).Msg("Registered WBNB/USDT pools for price conversion")
	}

	// Set CEX symbol to monitor
	m.cexStream.SetSymbols([]string{tc.CEXSymbol})

	log.Info().
		Str("symbol", tc.Symbol).
		Str("cex_symbol", tc.CEXSymbol).
		Int("pools", len(tc.Pools)).
		Msg("Monitor configured")

	// Fetch initial pool states + CEX price for new token
	go m.fetchPoolStates()
	go m.fetchInitialCEXPrice(context.Background(), tc.CEXSymbol, strings.TrimSuffix(tc.CEXSymbol, "USDT"))

	return nil
}

// Start begins monitoring. Blocks until ctx is cancelled.
func (m *Monitor) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = true
	ctx, m.cancel = context.WithCancel(ctx)
	m.mu.Unlock()

	wsURL := m.cfg.Chain.BSC.WsURL
	if wsURL == "" {
		wsURL = strings.Replace(m.cfg.Chain.BSC.RPC, "https://", "wss://", 1)
	}

	// Create LogWatcher with onSync callback
	m.logWatcher = engine.NewLogWatcher(wsURL, m.poolManager, m.onPoolUpdate)

	// Start CEX stream
	go func() {
		if err := m.cexStream.Start(ctx); err != nil {
			log.Error().Err(err).Msg("CEX stream exited")
		}
	}()

	// Start CEX price polling (triggers snapshot recalculation)
	go m.cexPriceLoop(ctx)

	// Start aggregator DEX price polling (accurate executable price)
	go m.aggregatorPriceLoop(ctx)

	// Fetch initial state for all pools (so we have a starting price)
	go m.fetchPoolStates()

	// Start LogWatcher (blocks until ctx cancelled)
	log.Info().Msg("Monitor started")
	return m.logWatcher.Start(ctx)
}

// Stop stops the monitor.
func (m *Monitor) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	m.cexStream.Stop()
	m.running = false
}

// IsRunning returns whether the monitor is active.
func (m *Monitor) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

// GetLatest returns the most recent price snapshot.
func (m *Monitor) GetLatest() PriceSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.latest
}

// Subscribe returns a channel that receives price updates.
func (m *Monitor) Subscribe() <-chan PriceSnapshot {
	ch := make(chan PriceSnapshot, 16)
	m.mu.Lock()
	m.subscribers = append(m.subscribers, ch)
	m.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (m *Monitor) Unsubscribe(ch <-chan PriceSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, sub := range m.subscribers {
		if sub == ch {
			m.subscribers = append(m.subscribers[:i], m.subscribers[i+1:]...)
			close(sub)
			return
		}
	}
}

// GetPoolManager returns the underlying pool manager (for strategy use).
func (m *Monitor) GetPoolManager() *engine.PoolManager {
	return m.poolManager
}

// GetCEXStream returns the underlying CEX stream (for strategy use).
func (m *Monitor) GetCEXStream() *cexstream.Stream {
	return m.cexStream
}

// onPoolUpdate is called when a DEX pool event (Sync/Swap) arrives.
func (m *Monitor) onPoolUpdate(poolAddr string) {
	m.recalcSnapshot()
}

// cexPriceLoop periodically checks for CEX price updates.
func (m *Monitor) cexPriceLoop(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastCEX decimal.Decimal
	initialFetched := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			tc := m.token
			m.mu.RUnlock()
			if tc == nil {
				continue
			}

			// Strip "USDT" suffix from CEXSymbol to get cache key
			cacheKey := strings.TrimSuffix(tc.CEXSymbol, "USDT")

			price, ok := m.cexStream.SpotCache.Get(cacheKey)

			// If no WebSocket price yet, fetch once via REST API
			if !ok && !initialFetched {
				initialFetched = true
				go m.fetchInitialCEXPrice(ctx, tc.CEXSymbol, cacheKey)
				continue
			}

			if ok && !price.Equal(lastCEX) {
				lastCEX = price
				m.recalcSnapshot()
			}
		}
	}
}

// aggregatorPriceLoop periodically fetches accurate DEX price from aggregator.
func (m *Monitor) aggregatorPriceLoop(ctx context.Context) {
	// Wait a bit for initial setup
	time.Sleep(3 * time.Second)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Fetch once immediately
	m.refreshAggregatorPrice()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshAggregatorPrice()
		}
	}
}

// refreshAggregatorPrice fetches the latest DEX price from aggregator and updates snapshot.
func (m *Monitor) refreshAggregatorPrice() {
	if m.aggregator == nil {
		return
	}

	m.mu.RLock()
	tc := m.token
	m.mu.RUnlock()
	if tc == nil {
		return
	}

	quote, err := m.aggregator.GetBestQuote(tc.ContractAddress, tc.Decimals, 100, true)
	if err != nil {
		log.Debug().Err(err).Msg("Aggregator price refresh failed")
		return
	}

	if quote.Price.IsPositive() {
		m.mu.Lock()
		m.aggDEXPrice = quote.Price
		m.mu.Unlock()
		m.recalcSnapshot()
	}
}

// fetchPoolStates fetches the on-chain state (reserves/slot0) for all registered pools.
func (m *Monitor) fetchPoolStates() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pools := m.poolManager.GetAllPoolAddresses()
	log.Info().Int("pools", len(pools)).Msg("Fetching initial pool states...")

	for _, addr := range pools {
		pool, ok := m.poolManager.GetPool(addr.Hex())
		if !ok {
			continue
		}

		if pool.Version == 3 {
			state, err := m.binAPI.FetchV3State(ctx, addr.Hex())
			if err != nil {
				log.Warn().Err(err).Str("pool", addr.Hex()).Msg("Failed to fetch V3 state")
				continue
			}
			m.poolManager.UpdateV3State(addr.Hex(), state.SqrtPriceX96, state.Liquidity, state.Tick)
		} else {
			r0, r1, err := m.binAPI.FetchV2Reserves(ctx, addr.Hex())
			if err != nil {
				log.Warn().Err(err).Str("pool", addr.Hex()).Msg("Failed to fetch V2 reserves")
				continue
			}
			m.poolManager.UpdateReserve(addr.Hex(), r0, r1)
		}
	}
	log.Info().Msg("Initial pool states fetched")
	m.recalcSnapshot()
}

// fetchInitialCEXPrice fetches the current price from Binance REST API and seeds the cache.
func (m *Monitor) fetchInitialCEXPrice(ctx context.Context, symbol, cacheKey string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := "https://api.binance.com/api/v3/ticker/price?symbol=" + symbol
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn().Err(err).Str("symbol", symbol).Msg("Failed to fetch initial CEX price")
		return
	}
	defer resp.Body.Close()

	var result struct {
		Price string `json:"price"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}

	price, err := decimal.NewFromString(result.Price)
	if err != nil || !price.IsPositive() {
		return
	}

	m.cexStream.SpotCache.Set(cacheKey, price)
	log.Info().Str("symbol", symbol).Str("price", price.StringFixed(6)).Msg("Initial CEX price fetched via REST")
	m.recalcSnapshot()
}

// recalcSnapshot recalculates and broadcasts the price snapshot.
func (m *Monitor) recalcSnapshot() {
	m.mu.RLock()
	tc := m.token
	m.mu.RUnlock()
	if tc == nil {
		return
	}

	// CEX price (raw from WebSocket, e.g., $5 for "1MBABYDOGEUSDT" = price of 1M tokens)
	cacheKey := strings.TrimSuffix(tc.CEXSymbol, "USDT")
	cexPrice, _ := m.cexStream.SpotCache.Get(cacheKey)

	// Normalize CEX price to per-single-token price
	// e.g., 1MBABYDOGE: $5 / 1000000 = $0.000005 per token
	// e.g., 1000SATS:   $0.05 / 1000 = $0.00005 per token
	// e.g., BNB:         $600 / 1 = $600 per token (no change)
	if tc.CEXMultiplier > 1 {
		cexPrice = cexPrice.Div(decimal.NewFromInt(tc.CEXMultiplier))
	}

	// DEX price: prefer aggregator price (accurate executable), fallback to pool price
	poolPrice := m.poolManager.GetPrice(tc.ContractAddress, tc.Decimals)

	m.mu.RLock()
	aggPrice := m.aggDEXPrice
	m.mu.RUnlock()

	// Use aggregator price if available, otherwise pool price
	dexPrice := aggPrice
	if !dexPrice.IsPositive() {
		dexPrice = poolPrice
	}

	// Pool liquidity (sum across all pools)
	liquidity := decimal.Zero
	for _, pool := range tc.Pools {
		liquidity = liquidity.Add(m.poolManager.GetPoolLiquidityUSD(pool.Address))
	}

	// Spread: (CEX - DEX) / DEX * 100  (positive = CEX more expensive)
	var spreadPct decimal.Decimal
	if cexPrice.IsPositive() && dexPrice.IsPositive() {
		spreadPct = cexPrice.Sub(dexPrice).Div(dexPrice).Mul(decimal.NewFromInt(100))
	}

	snap := PriceSnapshot{
		CEXPrice:      cexPrice,  // normalized per-token price
		DEXPrice:      dexPrice,  // best available per-token price
		PoolDEXPrice:  poolPrice, // raw pool price
		AggDEXPrice:   aggPrice,  // aggregator price
		SpreadPct:     spreadPct,
		PoolLiquidity: liquidity,
		Timestamp:     time.Now(),
	}

	m.mu.Lock()
	m.latest = snap
	subs := make([]chan PriceSnapshot, len(m.subscribers))
	copy(subs, m.subscribers)
	m.mu.Unlock()

	// Broadcast to subscribers (non-blocking)
	for _, ch := range subs {
		select {
		case ch <- snap:
		default:
		}
	}
}

// InitEthClient initializes the tax detector's eth client after creation.
// Called from main when we have the real ethclient.
func (m *Monitor) InitEthClient(client *ethclient.Client) {
	// The tax detector was created with nil client; in the newcoin flow we don't
	// need tax detection, so this is optional. The pool manager works fine without it.
}
