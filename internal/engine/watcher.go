package engine

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
)

// Sync 事件 Hash (PancakeSwap V2 Pair)
// event Sync(uint112 reserve0, uint112 reserve1)
var SyncEventHash = common.HexToHash("0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1")

// Swap 事件 Hash (PancakeSwap V3 Pool)
// PancakeSwap V3 比 Uniswap V3 多了 protocolFeesToken0, protocolFeesToken1
// event Swap(address indexed sender, address indexed recipient, int256 amount0, int256 amount1, uint160 sqrtPriceX96, uint128 liquidity, int24 tick, uint128 protocolFeesToken0, uint128 protocolFeesToken1)
var SwapV3EventHash = common.HexToHash("0x19b47279256b2a23a1665c810c8d55a1758940ee09377d4f8d26497a3577dc83")

// OnSyncCallback is called when a pool's reserves are updated.
// poolAddr is the LP pair address (lowercase hex).
type OnSyncCallback func(poolAddr string)

// LogWatcher 日志监听器
type LogWatcher struct {
	wsURL       string
	poolManager *PoolManager
	client      *ethclient.Client
	onSync      OnSyncCallback
	mu          sync.Mutex
	cancelSub   context.CancelFunc // to cancel current subscription for reload
}

// NewLogWatcher 创建日志监听器
func NewLogWatcher(wsURL string, pm *PoolManager, onSync OnSyncCallback) *LogWatcher {
	return &LogWatcher{
		wsURL:       wsURL,
		poolManager: pm,
		onSync:      onSync,
	}
}

// Start 启动监听 (带自动重连，指数退避)
func (w *LogWatcher) Start(ctx context.Context) error {
	backoff := 3 * time.Second
	const maxBackoff = 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			if err := w.subscribe(ctx); err != nil {
				log.Error().Err(err).Dur("retry_in", backoff).Msg("WebSocket 订阅中断，等待重连...")
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			// 连接成功后重置退避
			backoff = 3 * time.Second
		}
	}
}

// subscribe establishes a single WebSocket subscription lifecycle.
func (w *LogWatcher) subscribe(ctx context.Context) error {
	log.Info().Msg("正在连接 BSC WebSocket 节点...")

	client, err := ethclient.DialContext(ctx, w.wsURL)
	if err != nil {
		return err
	}
	defer client.Close()

	w.mu.Lock()
	w.client = client
	w.mu.Unlock()

	// 获取所有需要监听的池子地址
	addresses := w.poolManager.GetAllPoolAddresses()
	if len(addresses) == 0 {
		log.Warn().Msg("没有需要监听的池子，等待池子添加...")
		// Wait until pools are added or context is cancelled
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
			return nil // will reconnect and re-check
		}
	}

	// Create a sub-context so we can cancel on ReloadSubscription
	subCtx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.cancelSub = cancel
	w.mu.Unlock()
	defer cancel()

	query := ethereum.FilterQuery{
		Addresses: addresses,
		Topics:    [][]common.Hash{{SyncEventHash, SwapV3EventHash}},
	}

	logs := make(chan types.Log, 256)
	sub, err := client.SubscribeFilterLogs(subCtx, query, logs)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()

	log.Info().Int("pools", len(addresses)).Msg("WebSocket 事件监听已启动")

	for {
		select {
		case <-subCtx.Done():
			return nil
		case err := <-sub.Err():
			return err
		case vLog := <-logs:
			w.handleLog(vLog)
		}
	}
}

// handleLog 处理日志事件 — 根据 topic 分发到 V2 Sync 或 V3 Swap
func (w *LogWatcher) handleLog(vLog types.Log) {
	if len(vLog.Topics) == 0 {
		return
	}

	switch vLog.Topics[0] {
	case SyncEventHash:
		w.handleSyncLog(vLog)
	case SwapV3EventHash:
		w.handleSwapLog(vLog)
	}
}

// handleSyncLog 处理 V2 Sync 事件
func (w *LogWatcher) handleSyncLog(vLog types.Log) {
	if len(vLog.Data) < 64 {
		return
	}

	r0 := new(big.Int).SetBytes(vLog.Data[0:32])
	r1 := new(big.Int).SetBytes(vLog.Data[32:64])

	poolAddr := vLog.Address.Hex()

	log.Trace().Str("pool", poolAddr).Msg("收到 V2 Sync 事件")

	w.poolManager.UpdateReserve(poolAddr, r0, r1)

	if w.onSync != nil {
		w.onSync(poolAddr)
	}
}

// handleSwapLog 处理 PancakeSwap V3 Swap 事件
// data layout: amount0(int256) | amount1(int256) | sqrtPriceX96(uint160) | liquidity(uint128) | tick(int24) | protocolFeesToken0(uint128) | protocolFeesToken1(uint128)
// Each field is ABI-encoded as 32 bytes. We only need sqrtPriceX96, liquidity, tick (bytes [64:160]).
func (w *LogWatcher) handleSwapLog(vLog types.Log) {
	if len(vLog.Data) < 160 { // 至少需要前 5 个字段 (160 bytes), PCS V3 实际有 7 个 (224 bytes)
		return
	}

	// amount0 and amount1 are int256 (signed), but we don't need them for pricing
	// sqrtPriceX96: bytes [64:96]
	sqrtPriceX96 := new(big.Int).SetBytes(vLog.Data[64:96])

	// liquidity: bytes [96:128] — uint128
	liquidity := new(big.Int).SetBytes(vLog.Data[96:128])

	// tick: bytes [128:160] — int24 (signed, stored as int256)
	tickBig := new(big.Int).SetBytes(vLog.Data[128:160])
	// Convert uint256 to signed int256
	if tickBig.Bit(255) == 1 {
		// Negative: two's complement
		tickBig.Sub(tickBig, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	tick := int32(tickBig.Int64())

	poolAddr := vLog.Address.Hex()

	log.Trace().
		Str("pool", poolAddr).
		Str("sqrtPriceX96", sqrtPriceX96.String()).
		Str("liquidity", liquidity.String()).
		Int32("tick", tick).
		Msg("收到 V3 Swap 事件")

	w.poolManager.UpdateV3State(poolAddr, sqrtPriceX96, liquidity, tick)

	// V3 价格验证日志
	if symbol, ok := w.poolManager.GetSymbolByPool(poolAddr); ok {
		price := w.poolManager.GetPriceFromPool(poolAddr)
		log.Debug().
			Str("symbol", symbol).
			Str("pool", poolAddr[:10]).
			Str("V3价格", price.String()).
			Msg("V3 Swap → 价格更新")
	}

	if w.onSync != nil {
		w.onSync(poolAddr)
	}
}

// ReloadSubscription 重新加载订阅 (当新代币加入时调用)
// Cancels the current subscription, causing subscribe() to re-establish with new pool addresses.
func (w *LogWatcher) ReloadSubscription() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancelSub != nil {
		log.Info().Msg("重新加载 WebSocket 订阅...")
		w.cancelSub()
	}
}
