package newcoin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// TokenConfig holds the coin configuration for monitoring.
type TokenConfig struct {
	Symbol          string          `json:"symbol"`
	CEXSymbol       string          `json:"cex_symbol"`
	ContractAddress string          `json:"contract_address"`
	Decimals        int             `json:"decimals"`
	CEXMultiplier   int64           `json:"cex_multiplier"`
	Pools           []PoolInfoState `json:"pools,omitempty"`
}

// PoolInfoState stores a discovered pool's info for persistence.
type PoolInfoState struct {
	Address    string `json:"address"`
	Token0     string `json:"token0"`
	Token1     string `json:"token1"`
	Version    int    `json:"version"`
	FeeTier    uint32 `json:"fee_tier,omitempty"`
	QuoteToken string `json:"quote_token"` // "USDT" or "WBNB"
}

// HedgePosition tracks the current hedge state.
type HedgePosition struct {
	Active        bool    `json:"active"`
	CEXSpotQty    string  `json:"cex_spot_qty"`
	DEXTokenQty   string  `json:"dex_token_qty"`
	ShortQty      string  `json:"short_qty"`
	EntryPrice    string  `json:"entry_price"`
	InitAmountUSD float64 `json:"init_amount_usd"`
	OpenedAt      string  `json:"opened_at"`
	SpotOrderID   int64   `json:"spot_order_id"`
	ShortOrderID  string  `json:"short_order_id"`
	DEXSwapTxHash string  `json:"dex_swap_tx_hash"`
}

// Trade records a single arbitrage trade.
type Trade struct {
	ID          string    `json:"id"`
	Direction   string    `json:"direction"` // "DEX_TO_CEX" or "CEX_TO_DEX"
	Symbol      string    `json:"symbol"`
	AmountUSD   float64   `json:"amount_usd"`
	SpreadPct   float64   `json:"spread_pct"`
	ProfitUSD   float64   `json:"profit_usd"`
	Status      string    `json:"status"` // "pending", "buying", "transferring", "selling", "done", "failed"
	CEXPrice    string    `json:"cex_price"`
	DEXPrice    string    `json:"dex_price"`
	TxHash      string    `json:"tx_hash"`
	OrderID     string    `json:"order_id"`
	GasCostUSD  float64   `json:"gas_cost_usd"`
	CEXFeeUSD   float64   `json:"cex_fee_usd"`
	DEXFeeUSD   float64   `json:"dex_fee_usd"`
	TotalFeeUSD float64   `json:"total_fee_usd"`
	CreatedAt   time.Time `json:"created_at"`
	Error       string    `json:"error,omitempty"`
}

// Settings holds user-configurable parameters.
type Settings struct {
	AutoTrade        bool    `json:"auto_trade"`
	MinSpreadPct     float64 `json:"min_spread_pct"`
	TradeAmountUSD   float64 `json:"trade_amount_usd"`
	InitInventoryUSD float64 `json:"init_inventory_usd"`
	SlippagePct      float64 `json:"slippage_pct"`
}

// AppState is the top-level persisted state.
type AppState struct {
	Token    *TokenConfig   `json:"token"`
	Hedge    *HedgePosition `json:"hedge"`
	Trades   []Trade        `json:"trades"` // recent 20 for quick rendering
	Settings Settings       `json:"settings"`
	TotalPL  float64        `json:"total_pl"`
}

// StateManager handles thread-safe JSON persistence.
type StateManager struct {
	mu       sync.RWMutex
	state    AppState
	filePath string
	dataDir  string // directory for trade files (data/)
}

// NewStateManager loads or creates state from the given file path.
func NewStateManager(filePath string) *StateManager {
	sm := &StateManager{
		filePath: filePath,
		dataDir:  "data",
		state: AppState{
			Settings: Settings{
				MinSpreadPct:     0.5,
				TradeAmountUSD:   10,
				InitInventoryUSD: 200,
				SlippagePct:      1.0,
			},
		},
	}
	sm.load()
	return sm
}

func (sm *StateManager) load() {
	data, err := os.ReadFile(sm.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn().Err(err).Msg("failed to read state file")
		}
		return
	}
	if err := json.Unmarshal(data, &sm.state); err != nil {
		log.Warn().Err(err).Msg("failed to parse state file")
	}
}

// Save persists state to disk atomically.
func (sm *StateManager) Save() error {
	sm.mu.RLock()
	data, err := json.MarshalIndent(sm.state, "", "  ")
	sm.mu.RUnlock()
	if err != nil {
		return err
	}

	dir := filepath.Dir(sm.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmp := sm.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, sm.filePath)
}

// GetState returns a copy of the current state.
func (sm *StateManager) GetState() AppState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state
}

// SetToken updates the token configuration and saves.
func (sm *StateManager) SetToken(tc *TokenConfig) error {
	sm.mu.Lock()
	sm.state.Token = tc
	sm.mu.Unlock()
	return sm.Save()
}

// SetHedge updates the hedge position and saves.
func (sm *StateManager) SetHedge(h *HedgePosition) error {
	sm.mu.Lock()
	sm.state.Hedge = h
	sm.mu.Unlock()
	return sm.Save()
}

// AddTrade appends a trade to the in-memory recent list and persists to per-symbol file.
func (sm *StateManager) AddTrade(t Trade) error {
	sm.mu.Lock()
	sm.state.Trades = append(sm.state.Trades, t)
	// Keep only last 20 in memory
	if len(sm.state.Trades) > 20 {
		sm.state.Trades = sm.state.Trades[len(sm.state.Trades)-20:]
	}
	if t.Status == "done" {
		sm.state.TotalPL += t.ProfitUSD
	}
	sm.mu.Unlock()

	// Also save to per-symbol file
	if t.Symbol != "" {
		if err := sm.SaveTrade(t.Symbol, t); err != nil {
			log.Warn().Err(err).Str("symbol", t.Symbol).Msg("failed to save trade to file")
		}
	}

	return sm.Save()
}

// UpdateTrade updates an existing trade by ID in memory and in per-symbol file.
func (sm *StateManager) UpdateTrade(id string, fn func(*Trade)) error {
	sm.mu.Lock()
	var symbol string
	for i := range sm.state.Trades {
		if sm.state.Trades[i].ID == id {
			oldStatus := sm.state.Trades[i].Status
			fn(&sm.state.Trades[i])
			symbol = sm.state.Trades[i].Symbol
			// Update total P&L if trade just completed
			if oldStatus != "done" && sm.state.Trades[i].Status == "done" {
				sm.state.TotalPL += sm.state.Trades[i].ProfitUSD
			}
			break
		}
	}
	sm.mu.Unlock()

	// Also update in per-symbol file
	if symbol != "" {
		if err := sm.UpdateTradeInFile(symbol, id, fn); err != nil {
			log.Warn().Err(err).Str("symbol", symbol).Msg("failed to update trade in file")
		}
	}

	return sm.Save()
}

// SetSettings updates settings and saves.
func (sm *StateManager) SetSettings(s Settings) error {
	sm.mu.Lock()
	sm.state.Settings = s
	sm.mu.Unlock()
	return sm.Save()
}

// --- Per-symbol trade file storage ---

func (sm *StateManager) tradeFilePath(symbol string) string {
	return filepath.Join(sm.dataDir, "trades-"+symbol+".json")
}

// LoadTrades loads all trades for a symbol from its JSON file.
func (sm *StateManager) LoadTrades(symbol string) []Trade {
	fp := sm.tradeFilePath(symbol)
	data, err := os.ReadFile(fp)
	if err != nil {
		return nil
	}
	var trades []Trade
	if err := json.Unmarshal(data, &trades); err != nil {
		log.Warn().Err(err).Str("file", fp).Msg("failed to parse trade file")
		return nil
	}
	return trades
}

// SaveTrade appends a trade to the per-symbol JSON file.
func (sm *StateManager) SaveTrade(symbol string, t Trade) error {
	trades := sm.LoadTrades(symbol)
	trades = append(trades, t)
	return sm.writeTrades(symbol, trades)
}

// UpdateTradeInFile updates a trade in the per-symbol JSON file.
func (sm *StateManager) UpdateTradeInFile(symbol, id string, fn func(*Trade)) error {
	trades := sm.LoadTrades(symbol)
	for i := range trades {
		if trades[i].ID == id {
			fn(&trades[i])
			break
		}
	}
	return sm.writeTrades(symbol, trades)
}

// GetTradesByTime returns trades for a symbol within a time range.
func (sm *StateManager) GetTradesByTime(symbol string, from, to time.Time) []Trade {
	all := sm.LoadTrades(symbol)
	var result []Trade
	for _, t := range all {
		if (t.CreatedAt.Equal(from) || t.CreatedAt.After(from)) &&
			(t.CreatedAt.Equal(to) || t.CreatedAt.Before(to)) {
			result = append(result, t)
		}
	}
	return result
}

// GetRecentTrades returns the most recent N trades for a symbol.
func (sm *StateManager) GetRecentTrades(symbol string, limit int) []Trade {
	all := sm.LoadTrades(symbol)
	if limit <= 0 || limit >= len(all) {
		return all
	}
	// Sort by CreatedAt descending, then take first N
	sort.Slice(all, func(i, j int) bool {
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	return all[:limit]
}

func (sm *StateManager) writeTrades(symbol string, trades []Trade) error {
	if err := os.MkdirAll(sm.dataDir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(trades, "", "  ")
	if err != nil {
		return err
	}
	fp := sm.tradeFilePath(symbol)
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, fp)
}
