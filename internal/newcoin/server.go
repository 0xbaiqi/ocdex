package newcoin

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"ocdex/config"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server is the HTTP server for the NewCoin web UI.
type Server struct {
	cfg        *config.Config
	state      *StateManager
	monitor    *Monitor
	strategy   *Strategy
	binAPI     *BinanceAPI
	autoTrader *AutoTrader
	tmpl       *template.Template
	mux        *http.ServeMux
	monCtx     context.Context
	monCancel  context.CancelFunc
}

// NewServer creates a new web server.
func NewServer(cfg *config.Config, state *StateManager, monitor *Monitor, strategy *Strategy, binAPI *BinanceAPI, autoTrader *AutoTrader) *Server {
	funcMap := template.FuncMap{
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Format("15:04:05")
		},
		"formatFloat": func(f float64, prec int) string {
			return strconv.FormatFloat(f, 'f', prec, 64)
		},
		"formatDecimal": func(d decimal.Decimal, prec int32) string {
			return d.StringFixed(prec)
		},
	}

	tmpl := template.Must(template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html"))

	monCtx, monCancel := context.WithCancel(context.Background())

	s := &Server{
		cfg:        cfg,
		state:      state,
		monitor:    monitor,
		strategy:   strategy,
		binAPI:     binAPI,
		autoTrader: autoTrader,
		tmpl:       tmpl,
		mux:        http.NewServeMux(),
		monCtx:     monCtx,
		monCancel:  monCancel,
	}

	s.routes()
	return s
}

func (s *Server) routes() {
	// Static files
	staticSub, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Pages
	s.mux.HandleFunc("/", s.handleIndex)

	// SSE
	s.mux.HandleFunc("/sse", s.handleSSE)

	// API - unified
	s.mux.HandleFunc("/api/config", s.handleConfig)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
	s.mux.HandleFunc("/api/start", s.handleStart)
	s.mux.HandleFunc("/api/stop", s.handleStop)
	s.mux.HandleFunc("/api/positions", s.handlePositions)
	s.mux.HandleFunc("/api/cost-estimate", s.handleCostEstimate)
	s.mux.HandleFunc("/api/trades", s.handleTrades)
	s.mux.HandleFunc("/api/auto-trade", s.handleAutoTrade)

	// Trade operations (kept)
	s.mux.HandleFunc("/api/trade/dex-to-cex", s.handleDEXtoCEX)
	s.mux.HandleFunc("/api/trade/cex-buy", s.handleCEXBuy)
	s.mux.HandleFunc("/api/trade/dex-sell", s.handleDEXSell)

	// Utilities (kept)
	s.mux.HandleFunc("/api/binance/coins", s.handleBinanceCoins)
	s.mux.HandleFunc("/api/pool/discover", s.handlePoolDiscover)
	s.mux.HandleFunc("/api/aggregator/quote", s.handleAggregatorQuote)
	s.mux.HandleFunc("/api/close-all", s.handleCloseAll)
}

// Start starts the HTTP server on the given address.
func (s *Server) Start(addr string) error {
	log.Info().Str("addr", addr).Msg("NewCoin web server starting")
	return http.ListenAndServe(addr, s.mux)
}

// handleIndex renders the main page.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := map[string]any{
		"State":   s.state.GetState(),
		"Running": s.monitor.IsRunning(),
		"Wallet":  s.strategy.GetWalletAddress(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		log.Error().Err(err).Msg("template render error")
		http.Error(w, err.Error(), 500)
	}
}

// handleSSE streams price updates via Server-Sent Events.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.monitor.Subscribe()
	defer s.monitor.Unsubscribe(ch)

	ctx := r.Context()

	// Send current state immediately
	snap := s.monitor.GetLatest()
	s.writeSSEEvent(w, flusher, snap)

	for {
		select {
		case <-ctx.Done():
			return
		case snap, ok := <-ch:
			if !ok {
				return
			}
			s.writeSSEEvent(w, flusher, snap)
		}
	}
}

func (s *Server) writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, snap PriceSnapshot) {
	data := map[string]string{
		"cex_price":  snap.CEXPrice.StringFixed(6),
		"dex_price":  snap.DEXPrice.StringFixed(6),
		"spread_pct": snap.SpreadPct.StringFixed(3),
		"liquidity":  snap.PoolLiquidity.StringFixed(0),
		"timestamp":  snap.Timestamp.Format("15:04:05"),
	}
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	flusher.Flush()
}

// handleConfig saves token configuration and auto-discovers pools.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	r.ParseMultipartForm(32 << 20)

	cexMultiplier, _ := strconv.ParseInt(r.FormValue("cex_multiplier"), 10, 64)
	if cexMultiplier == 0 {
		cexMultiplier = 1
	}

	tc := &TokenConfig{
		Symbol:          r.FormValue("symbol"),
		CEXSymbol:       r.FormValue("cex_symbol"),
		ContractAddress: r.FormValue("contract_address"),
		CEXMultiplier:   cexMultiplier,
	}

	if tc.Symbol == "" || tc.ContractAddress == "" {
		s.jsonError(w, "symbol and contract_address are required", 400)
		return
	}

	// Auto-detect decimals
	ctx := r.Context()
	decimals, err := s.binAPI.FetchDecimals(ctx, tc.ContractAddress)
	if err != nil {
		decimals = 18
	}
	tc.Decimals = decimals

	// Auto-discover pools (USDT + WBNB)
	pools, err := s.binAPI.DiscoverPools(ctx, tc.ContractAddress)
	if err != nil {
		s.jsonError(w, "Pool discovery failed: "+err.Error(), 500)
		return
	}
	if len(pools) == 0 {
		s.jsonError(w, "No USDT or WBNB pools found for this token", 400)
		return
	}

	for _, p := range pools {
		tc.Pools = append(tc.Pools, PoolInfoState{
			Address:    p.Address,
			Token0:     p.Token0,
			Token1:     p.Token1,
			Version:    p.Version,
			FeeTier:    p.FeeTier,
			QuoteToken: p.QuoteToken,
		})
	}

	if err := s.state.SetToken(tc); err != nil {
		s.jsonError(w, err.Error(), 500)
		return
	}

	// Configure monitor
	if err := s.monitor.Configure(tc); err != nil {
		s.jsonError(w, err.Error(), 500)
		return
	}

	s.jsonOKWithData(w, fmt.Sprintf("已保存配置，发现 %d 个池子", len(pools)), map[string]any{
		"pools":    pools,
		"decimals": decimals,
	})
}

// handleStart saves settings, initializes positions, and starts monitoring.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	r.ParseMultipartForm(32 << 20)

	// Parse and save settings
	settings := Settings{
		AutoTrade:        r.FormValue("auto_trade") == "on" || r.FormValue("auto_trade") == "true",
		MinSpreadPct:     parseFloat(r.FormValue("min_spread_pct"), 0.5),
		TradeAmountUSD:   parseFloat(r.FormValue("trade_amount_usd"), 10),
		InitInventoryUSD: parseFloat(r.FormValue("init_inventory_usd"), 200),
		SlippagePct:      parseFloat(r.FormValue("slippage_pct"), 1.0),
	}
	if err := s.state.SetSettings(settings); err != nil {
		s.jsonError(w, err.Error(), 500)
		return
	}

	tc := s.state.GetState().Token
	if tc == nil {
		s.jsonError(w, "No token configured", 400)
		return
	}

	// Start monitor if not running
	if !s.monitor.IsRunning() {
		s.monitor.Configure(tc)
		s.monCtx, s.monCancel = context.WithCancel(context.Background())
		go func() {
			if err := s.monitor.Start(s.monCtx); err != nil {
				log.Error().Err(err).Msg("Monitor exited")
			}
		}()
	}

	// Initialize positions (CEX buy + DEX buy + short)
	if err := s.strategy.InitPositions(settings.InitInventoryUSD); err != nil {
		s.jsonError(w, "建仓失败: "+err.Error(), 500)
		return
	}

	// Start auto-trader if AutoTrade is enabled
	if settings.AutoTrade {
		go s.autoTrader.Start(s.monCtx)
	}

	s.jsonOK(w, "建仓成功，监控已启动")
}

// handleStop closes all positions and stops monitoring.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	// Stop auto-trader
	s.autoTrader.Stop()

	// Close all positions
	if err := s.strategy.CloseAllPositions(); err != nil {
		s.jsonError(w, "平仓失败: "+err.Error(), 500)
		return
	}

	// Stop monitor
	s.monitor.Stop()

	s.jsonOK(w, "已平仓并停止监控")
}

// handleCloseAll closes all positions without stopping the monitor.
func (s *Server) handleCloseAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	log.Info().Msg("一键清仓: closing all positions")

	if err := s.strategy.CloseAllPositions(); err != nil {
		log.Error().Err(err).Msg("一键清仓失败")
		s.jsonError(w, "清仓失败: "+err.Error(), 500)
		return
	}

	s.jsonOK(w, "已清仓完成：CEX现货+DEX代币已卖出，空单已平仓")
}

// handleSettings updates user settings (save only, no position changes).
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	r.ParseMultipartForm(32 << 20)
	settings := Settings{
		AutoTrade:        r.FormValue("auto_trade") == "on" || r.FormValue("auto_trade") == "true",
		MinSpreadPct:     parseFloat(r.FormValue("min_spread_pct"), 0.5),
		TradeAmountUSD:   parseFloat(r.FormValue("trade_amount_usd"), 10),
		InitInventoryUSD: parseFloat(r.FormValue("init_inventory_usd"), 200),
		SlippagePct:      parseFloat(r.FormValue("slippage_pct"), 1.0),
	}

	if err := s.state.SetSettings(settings); err != nil {
		s.jsonError(w, err.Error(), 500)
		return
	}

	// Start/stop auto trader based on setting change
	if settings.AutoTrade && !s.autoTrader.IsRunning() && s.monitor.IsRunning() {
		go s.autoTrader.Start(s.monCtx)
	} else if !settings.AutoTrade && s.autoTrader.IsRunning() {
		s.autoTrader.Stop()
	}

	s.jsonOK(w, "Settings saved")
}

// handleAutoTrade toggles auto-trading on/off.
func (s *Server) handleAutoTrade(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// Return current auto-trade status
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":     "ok",
			"auto_trade": s.state.GetState().Settings.AutoTrade,
			"running":    s.autoTrader.IsRunning(),
		})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	r.ParseMultipartForm(32 << 20)
	enable := r.FormValue("enable") == "true" || r.FormValue("enable") == "on"

	// Update settings
	settings := s.state.GetState().Settings
	settings.AutoTrade = enable
	if err := s.state.SetSettings(settings); err != nil {
		s.jsonError(w, err.Error(), 500)
		return
	}

	if enable {
		if !s.monitor.IsRunning() {
			s.jsonError(w, "Monitor not running. Please start monitoring first.", 400)
			return
		}
		st := s.state.GetState()
		if st.Hedge == nil || !st.Hedge.Active {
			s.jsonError(w, "No active hedge. Please initialize positions first.", 400)
			return
		}
		if !s.autoTrader.IsRunning() {
			go s.autoTrader.Start(s.monCtx)
		}
		s.jsonOK(w, "Auto-trade enabled")
	} else {
		s.autoTrader.Stop()
		s.jsonOK(w, "Auto-trade disabled")
	}
}

// handlePositions returns the complete position overview.
func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	positions, err := s.strategy.GetPositions()
	if err != nil {
		s.jsonError(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": positions})
}

// handleCostEstimate returns fee estimates for both arbitrage directions.
func (s *Server) handleCostEstimate(w http.ResponseWriter, r *http.Request) {
	amountStr := r.URL.Query().Get("amount")
	amount, _ := strconv.ParseFloat(amountStr, 64)
	if amount <= 0 {
		amount = s.state.GetState().Settings.TradeAmountUSD
	}

	est := s.strategy.EstimateTradeCost(amount)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": est})
}

// handleTrades returns trades for a specific symbol.
func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		tc := s.state.GetState().Token
		if tc != nil {
			symbol = tc.Symbol
		}
	}

	limitStr := r.URL.Query().Get("limit")
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 {
		limit = 50
	}

	var trades []Trade
	if symbol != "" {
		trades = s.state.GetRecentTrades(symbol, limit)
	} else {
		trades = s.state.GetState().Trades
	}
	if trades == nil {
		trades = []Trade{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "trades": trades})
}

// handleDEXtoCEX executes direction A: buy on DEX, sell on CEX.
func (s *Server) handleDEXtoCEX(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	r.ParseMultipartForm(32 << 20)
	amount, _ := strconv.ParseFloat(r.FormValue("amount"), 64)
	if amount <= 0 {
		amount = s.state.GetState().Settings.TradeAmountUSD
	}

	trade, err := s.strategy.ExecuteDEXtoCEX(amount)
	if err != nil {
		s.jsonErrorWithData(w, err.Error(), trade, 500)
		return
	}
	s.jsonOKWithData(w, "Trade executed", trade)
}

// handleCEXBuy executes direction B step 1: buy on CEX.
func (s *Server) handleCEXBuy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	r.ParseMultipartForm(32 << 20)
	amount, _ := strconv.ParseFloat(r.FormValue("amount"), 64)
	if amount <= 0 {
		amount = s.state.GetState().Settings.TradeAmountUSD
	}

	trade, err := s.strategy.ExecuteCEXtoDEX_Buy(amount)
	if err != nil {
		s.jsonErrorWithData(w, err.Error(), trade, 500)
		return
	}
	s.jsonOKWithData(w, "CEX buy executed, please withdraw manually", trade)
}

// handleDEXSell executes direction B step 2: sell on DEX after manual withdrawal.
func (s *Server) handleDEXSell(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	r.ParseMultipartForm(32 << 20)
	tradeID := r.FormValue("trade_id")
	if tradeID == "" {
		s.jsonError(w, "trade_id required", 400)
		return
	}

	trade, err := s.strategy.ExecuteCEXtoDEX_Sell(tradeID)
	if err != nil {
		s.jsonErrorWithData(w, err.Error(), trade, 500)
		return
	}
	s.jsonOKWithData(w, "DEX sell executed", trade)
}

// handleBinanceCoins returns BSC-enabled coins from Binance with 24h market data.
func (s *Server) handleBinanceCoins(w http.ResponseWriter, r *http.Request) {
	coins, err := s.binAPI.FetchBSCCoins(r.Context())
	if err != nil {
		s.jsonError(w, "Failed to fetch coins: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "coins": coins})
}

// handlePoolDiscover discovers DEX pools for a given contract address.
func (s *Server) handlePoolDiscover(w http.ResponseWriter, r *http.Request) {
	contract := r.URL.Query().Get("contract")
	if contract == "" {
		s.jsonError(w, "contract parameter required", 400)
		return
	}

	pools, err := s.binAPI.DiscoverPools(r.Context(), contract)
	if err != nil {
		s.jsonError(w, "Pool discovery failed: "+err.Error(), 500)
		return
	}

	// Also fetch decimals
	decimals, err := s.binAPI.FetchDecimals(r.Context(), contract)
	if err != nil {
		decimals = 18 // fallback
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"pools":    pools,
		"decimals": decimals,
	})
}

// --- JSON response helpers ---

func (s *Server) jsonOK(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": msg})
}

func (s *Server) jsonOKWithData(w http.ResponseWriter, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "message": msg, "data": data})
}

func (s *Server) jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": msg})
}

func (s *Server) jsonErrorWithData(w http.ResponseWriter, msg string, data any, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": msg, "data": data})
}

func parseFloat(s string, fallback float64) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

// handleAggregatorQuote returns aggregated DEX quotes from 1inch + PancakeSwap V3.
func (s *Server) handleAggregatorQuote(w http.ResponseWriter, r *http.Request) {
	tc := s.state.GetState().Token
	if tc == nil {
		s.jsonError(w, "no token configured", 400)
		return
	}

	amountUSD := parseFloat(r.URL.Query().Get("amount"), 100)

	if s.strategy.aggregator == nil {
		s.jsonError(w, "aggregator not initialized", 500)
		return
	}

	quote, err := s.strategy.aggregator.GetBestQuote(
		tc.ContractAddress, tc.Decimals, amountUSD, true,
	)
	if err != nil {
		s.jsonError(w, "aggregator quote failed: "+err.Error(), 500)
		return
	}

	snap := s.monitor.GetLatest()

	s.jsonOKWithData(w, "aggregator quote", map[string]any{
		"source":        quote.Source,
		"amount_in_usd": amountUSD,
		"token_out":     quote.TokenOut.StringFixed(6),
		"price":         quote.Price.StringFixed(8),
		"pool_dex":      snap.DEXPrice.StringFixed(8),
		"cex_price":     snap.CEXPrice.StringFixed(8),
		"spread_pct":    snap.CEXPrice.Sub(quote.Price).Div(quote.Price).Mul(decimal.NewFromInt(100)).StringFixed(3),
	})
}
