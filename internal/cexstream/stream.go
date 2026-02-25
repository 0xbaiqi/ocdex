package cexstream

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// PriceCache stores the latest CEX price per symbol.
type PriceCache struct {
	mu       sync.RWMutex
	prices   map[string]decimal.Decimal // key: symbol (e.g., "BNB"), value: USDT price
	OnUpdate func(symbol string)        // 价格更新回调 (可选)
}

// Get returns the cached price for a symbol.
func (c *PriceCache) Get(symbol string) (decimal.Decimal, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.prices[symbol]
	return p, ok
}

// Set stores a price for a symbol and triggers the OnUpdate callback.
func (c *PriceCache) Set(symbol string, price decimal.Decimal) {
	c.mu.Lock()
	c.prices[symbol] = price
	cb := c.OnUpdate
	c.mu.Unlock()

	if cb != nil {
		cb(symbol)
	}
}

func newPriceCache() *PriceCache {
	return &PriceCache{prices: make(map[string]decimal.Decimal)}
}

// TickerData 币安行情数据
type TickerData struct {
	Symbol    string `json:"s"` // e.g., "BNBUSDT"
	LastPrice string `json:"c"` // latest price
}

// wsConn wraps a single WebSocket connection with auto-reconnect
type wsConn struct {
	wsURL   string
	symbols map[string]bool
	mu      sync.RWMutex
	conn    *websocket.Conn
	cache   *PriceCache
	running bool
	name    string // for logging ("现货" / "合约")
}

func newWsConn(wsURL, name string, cache *PriceCache) *wsConn {
	return &wsConn{
		wsURL:   wsURL,
		symbols: make(map[string]bool),
		cache:   cache,
		name:    name,
	}
}

func (w *wsConn) setSymbols(symbols []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.symbols = make(map[string]bool)
	for _, sym := range symbols {
		w.symbols[sym] = true
	}
}

func (w *wsConn) start(ctx context.Context) error {
	w.running = true
	for w.running {
		select {
		case <-ctx.Done():
			w.running = false
			return nil
		default:
			if err := w.connect(ctx); err != nil {
				log.Error().Err(err).Str("type", w.name).Msg("WebSocket 连接失败，5秒后重试")
				time.Sleep(5 * time.Second)
				continue
			}
			w.readLoop(ctx)
		}
	}
	return nil
}

func (w *wsConn) connect(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, w.wsURL, nil)
	if err != nil {
		return err
	}
	w.conn = conn
	log.Info().Str("type", w.name).Msg("CEX WebSocket 已连接")
	return nil
}

func (w *wsConn) readLoop(ctx context.Context) {
	defer func() {
		if w.conn != nil {
			w.conn.Close()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			_, message, err := w.conn.ReadMessage()
			if err != nil {
				log.Warn().Err(err).Str("type", w.name).Msg("WebSocket 读取失败")
				return
			}
			w.processMessage(message)
		}
	}
}

func (w *wsConn) processMessage(message []byte) {
	var tickers []TickerData
	if err := json.Unmarshal(message, &tickers); err != nil {
		return
	}

	w.mu.RLock()
	defer w.mu.RUnlock()

	for _, ticker := range tickers {
		if !w.symbols[ticker.Symbol] {
			continue
		}
		price, err := decimal.NewFromString(ticker.LastPrice)
		if err != nil {
			continue
		}
		// "BNBUSDT" -> "BNB"
		symbol := ticker.Symbol
		if len(symbol) > 4 && symbol[len(symbol)-4:] == "USDT" {
			symbol = symbol[:len(symbol)-4]
		}
		w.cache.Set(symbol, price)
	}
}

func (w *wsConn) stop() {
	w.running = false
	if w.conn != nil {
		w.conn.Close()
	}
}

// Stream manages both spot and futures WebSocket price feeds.
type Stream struct {
	spot    *wsConn
	futures *wsConn
	// SpotCache holds spot prices (used for selling on CEX after deposit)
	SpotCache *PriceCache
	// FuturesCache holds futures prices (used for opportunity detection & shorting)
	FuturesCache *PriceCache
}

// NewStream creates a new CEX price stream with both spot and futures feeds.
func NewStream() *Stream {
	spotCache := newPriceCache()
	futuresCache := newPriceCache()
	return &Stream{
		spot:         newWsConn("wss://stream.binance.com/ws/!miniTicker@arr", "现货", spotCache),
		futures:      newWsConn("wss://fstream.binance.com/ws/!miniTicker@arr", "合约", futuresCache),
		SpotCache:    spotCache,
		FuturesCache: futuresCache,
	}
}

// SetSymbols sets the CEX symbols to monitor (e.g., ["BNBUSDT", "ETHUSDT"]).
// Both spot and futures will monitor the same symbols.
func (s *Stream) SetSymbols(symbols []string) {
	s.spot.setSymbols(symbols)
	s.futures.setSymbols(symbols)
}

// Start connects both spot and futures WebSocket feeds.
func (s *Stream) Start(ctx context.Context) error {
	// Start futures in a goroutine
	go func() {
		if err := s.futures.start(ctx); err != nil {
			log.Error().Err(err).Msg("合约 WebSocket 退出")
		}
	}()

	// Spot runs in the caller's goroutine
	return s.spot.start(ctx)
}

// Stop stops both connections.
func (s *Stream) Stop() {
	s.spot.stop()
	s.futures.stop()
}
