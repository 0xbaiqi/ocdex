package discovery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ocdex/config"
	"ocdex/internal/registry"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
)

// PancakeSwap V2 Factory ABI (getPair only)
const FactoryV2ABI = `[{"constant":true,"inputs":[{"name":"tokenA","type":"address"},{"name":"tokenB","type":"address"}],"name":"getPair","outputs":[{"name":"pair","type":"address"}],"stateMutability":"view","type":"function"}]`

// PancakeSwap V3 Factory ABI (getPool only)
const FactoryV3ABI = `[{"inputs":[{"internalType":"address","name":"tokenA","type":"address"},{"internalType":"address","name":"tokenB","type":"address"},{"internalType":"uint24","name":"fee","type":"uint24"}],"name":"getPool","outputs":[{"internalType":"address","name":"pool","type":"address"}],"stateMutability":"view","type":"function"}]`

// V3 Pool ABI: slot0 for price state, liquidity for current range liquidity, token0/token1
const V3PoolABI = `[{"inputs":[],"name":"slot0","outputs":[{"internalType":"uint160","name":"sqrtPriceX96","type":"uint160"},{"internalType":"int24","name":"tick","type":"int24"},{"internalType":"uint16","name":"observationIndex","type":"uint16"},{"internalType":"uint16","name":"observationCardinality","type":"uint16"},{"internalType":"uint16","name":"observationCardinalityNext","type":"uint16"},{"internalType":"uint32","name":"feeProtocol","type":"uint32"},{"internalType":"bool","name":"unlocked","type":"bool"}],"stateMutability":"view","type":"function"},{"inputs":[],"name":"liquidity","outputs":[{"internalType":"uint128","name":"","type":"uint128"}],"stateMutability":"view","type":"function"},{"inputs":[],"name":"token0","outputs":[{"internalType":"address","name":"","type":"address"}],"stateMutability":"view","type":"function"},{"inputs":[],"name":"token1","outputs":[{"internalType":"address","name":"","type":"address"}],"stateMutability":"view","type":"function"}]`

// V3FeeTiers are the fee tiers supported by PancakeSwap V3
var V3FeeTiers = []uint32{100, 500, 2500, 10000}

// PairABI for token0/token1 queries
const PairABI = `[{"constant":true,"inputs":[],"name":"token0","outputs":[{"name":"","type":"address"}],"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"token1","outputs":[{"name":"","type":"address"}],"stateMutability":"view","type":"function"}]`

// PoolInfo holds LP pool address and its token ordering
type PoolInfo struct {
	PairAddress string
	Token0      string
	Token1      string
	Version     int    // 2 or 3
	FeeTier     uint32 // V3: 100/500/2500/10000
}

// TokenMetadata 本地缓存的代币元数据
type TokenMetadata struct {
	Symbol          string `json:"symbol"`
	ContractAddress string `json:"address"`
	Decimals        int    `json:"decimals"`
	UpdatedAt       int64  `json:"updated_at"`
}

// BinanceDiscovery 币安币种发现服务
type BinanceDiscovery struct {
	apiKey    string
	secretKey string
	client    *http.Client
	ethClient *ethclient.Client
	cacheFile string
	cache     map[string]TokenMetadata
	cacheMu   sync.RWMutex
}

// CoinInfo 币安币种信息
type CoinInfo struct {
	Coin        string        `json:"coin"`
	Name        string        `json:"name"`
	NetworkList []NetworkInfo `json:"networkList"`
}

// NetworkInfo 网络信息
type NetworkInfo struct {
	Network         string `json:"network"`
	Coin            string `json:"coin"`
	ContractAddress string `json:"contractAddress"`
	WithdrawEnable  bool   `json:"withdrawEnable"`
	DepositEnable   bool   `json:"depositEnable"`
}

// SymbolInfo 交易对信息
type SymbolInfo struct {
	Symbol     string `json:"symbol"`
	BaseAsset  string `json:"baseAsset"`
	QuoteAsset string `json:"quoteAsset"`
	Status     string `json:"status"`
}

// ExchangeInfo 交易所信息
type ExchangeInfo struct {
	Symbols []SymbolInfo `json:"symbols"`
}

// NewBinanceDiscovery 创建发现服务
func NewBinanceDiscovery(cfg config.APIConfig, ethClient *ethclient.Client) *BinanceDiscovery {
	d := &BinanceDiscovery{
		apiKey:    cfg.ApiKey,
		secretKey: cfg.SecretKey,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		ethClient: ethClient,
		cacheFile: "tokens.json",
		cache:     make(map[string]TokenMetadata),
	}
	d.loadCache()
	return d
}

// DiscoverBSCTokens 发现 BSC 链上的币种
func (d *BinanceDiscovery) DiscoverBSCTokens(ctx context.Context) ([]*registry.TokenInfo, error) {
	// Step 1: 获取支持 USDT 交易对的币种
	usdtSymbols, err := d.getUSDTSymbols(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取交易对失败: %w", err)
	}
	log.Info().Int("数量", len(usdtSymbols)).Msg("📊 发现 USDT 交易对")

	// Step 2: 获取币种详情
	coinInfos, err := d.getCoinInfos(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("⚠️ API 获取详情失败，尝试使用缓存数据")
		// 如果 API 失败，尝试仅返回缓存中的币种
		return d.getTokensFromCache(), nil
	}

	// Step 3: 筛选并验证 BSC 币种
	var wg sync.WaitGroup
	tokenChan := make(chan *registry.TokenInfo, len(coinInfos))
	sem := make(chan struct{}, 10) // 并发限制 10

	// 跳过稳定币: 它们和 USDT 之间的价差不是真正的套利机会
	stablecoins := map[string]bool{
		"USDC": true, "BUSD": true, "DAI": true, "TUSD": true,
		"USDP": true, "FDUSD": true, "USDD": true, "PYUSD": true,
	}

	for _, coin := range coinInfos {
		if stablecoins[coin.Coin] {
			continue
		}

		cexSymbol := coin.Coin + "USDT"
		if _, ok := usdtSymbols[cexSymbol]; !ok {
			continue
		}

		// 查找 BSC 网络
		var bscNetwork *NetworkInfo
		for _, network := range coin.NetworkList {
			// 必须是 BSC 且有合约地址
			if network.Network == "BSC" && network.ContractAddress != "" {
				bscNetwork = &network
				break
			}
		}

		if bscNetwork == nil {
			continue
		}

		wg.Add(1)
		go func(c CoinInfo, n NetworkInfo, symbol string) {
			defer wg.Done()
			sem <- struct{}{}        // 获取令牌
			defer func() { <-sem }() // 释放令牌

			// 检查缓存
			d.cacheMu.RLock()
			cached, hit := d.cache[c.Coin]
			d.cacheMu.RUnlock()

			decimals := 18
			// 如果缓存命中且地址一致，直接使用缓存精度
			if hit && strings.EqualFold(cached.ContractAddress, n.ContractAddress) {
				decimals = cached.Decimals
			} else {
				// 否则链上查询
				realDecimals, err := d.fetchOnChainDecimals(ctx, n.ContractAddress)
				if err != nil {
					log.Warn().Str("coin", c.Coin).Err(err).Msg("获取精度失败, 跳过")
					return
				}
				decimals = realDecimals

				// 更新缓存
				d.updateCache(c.Coin, n.ContractAddress, decimals)
			}

			tokenChan <- &registry.TokenInfo{
				Symbol:          c.Coin,
				Name:            c.Name,
				Chain:           "BSC",
				ContractAddress: n.ContractAddress,
				CEXSymbol:       symbol,
				Decimals:        decimals,
				CEXMultiplier:   registry.ParseCEXMultiplier(c.Coin),
				HasLiquidity:    true,
			}
		}(coin, *bscNetwork, cexSymbol)
	}

	wg.Wait()
	close(tokenChan)

	// 保存更新后的缓存
	d.saveCache()

	tokens := make([]*registry.TokenInfo, 0)
	for t := range tokenChan {
		tokens = append(tokens, t)
	}

	log.Info().Int("数量", len(tokens)).Msg("✅ 发现并验证 BSC 币种")
	return tokens, nil
}

// fetchOnChainDecimals 从链上获取精度
func (d *BinanceDiscovery) fetchOnChainDecimals(ctx context.Context, address string) (int, error) {
	// method signature for decimals(): 0x313ce567
	callData := common.Hex2Bytes("313ce567")
	contractAddr := common.HexToAddress(address)

	msg := ethereum.CallMsg{
		To:   &contractAddr,
		Data: callData,
	}

	result, err := d.ethClient.CallContract(ctx, msg, nil)
	if err != nil {
		return 0, err
	}

	if len(result) == 0 {
		return 0, fmt.Errorf("返回为空")
	}

	decimals := new(big.Int).SetBytes(result)
	return int(decimals.Int64()), nil
}

// updateCache 更新单个缓存
func (d *BinanceDiscovery) updateCache(symbol, address string, decimals int) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	d.cache[symbol] = TokenMetadata{
		Symbol:          symbol,
		ContractAddress: address,
		Decimals:        decimals,
		UpdatedAt:       time.Now().Unix(),
	}
}

// loadCache 加载缓存
func (d *BinanceDiscovery) loadCache() {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()

	file, err := os.ReadFile(d.cacheFile)
	if err != nil {
		return // 文件不存在或读取失败忽略
	}

	var data map[string]TokenMetadata
	if err := json.Unmarshal(file, &data); err == nil {
		d.cache = data
		log.Info().Int("数量", len(d.cache)).Msg("📦 加载本地代币元数据")
	}
}

// saveCache 保存缓存
func (d *BinanceDiscovery) saveCache() {
	d.cacheMu.RLock()
	defer d.cacheMu.RUnlock()

	data, err := json.MarshalIndent(d.cache, "", "  ")
	if err != nil {
		log.Error().Err(err).Msg("序列化缓存失败")
		return
	}

	if err := os.WriteFile(d.cacheFile, data, 0644); err != nil {
		log.Error().Err(err).Msg("保存缓存文件失败")
	}
}

// getTokensFromCache 从缓存构建 TokenInfo
func (d *BinanceDiscovery) getTokensFromCache() []*registry.TokenInfo {
	d.cacheMu.RLock()
	defer d.cacheMu.RUnlock()

	tokens := make([]*registry.TokenInfo, 0, len(d.cache))
	for _, meta := range d.cache {
		tokens = append(tokens, &registry.TokenInfo{
			Symbol:          meta.Symbol,
			Name:            meta.Symbol,
			Chain:           "BSC",
			ContractAddress: meta.ContractAddress,
			CEXSymbol:       meta.Symbol + "USDT",
			Decimals:        meta.Decimals,
			CEXMultiplier:   registry.ParseCEXMultiplier(meta.Symbol),
			HasLiquidity:    true,
		})
	}
	return tokens
}

// getUSDTSymbols 获取所有 USDT 交易对
func (d *BinanceDiscovery) getUSDTSymbols(ctx context.Context) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.binance.com/api/v3/exchangeInfo", nil)
	if err != nil {
		return nil, err
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var info ExchangeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}

	symbols := make(map[string]bool)
	for _, s := range info.Symbols {
		if s.QuoteAsset == "USDT" && s.Status == "TRADING" {
			symbols[s.Symbol] = true
		}
	}
	return symbols, nil
}

// getCoinInfos 获取币种详情 (需要 API Key 和签名)
func (d *BinanceDiscovery) getCoinInfos(ctx context.Context) ([]CoinInfo, error) {
	if d.apiKey == "" || d.secretKey == "" {
		return nil, fmt.Errorf("未配置 API Key")
	}

	timestamp := time.Now().UnixMilli()
	params := fmt.Sprintf("timestamp=%d", timestamp)
	signature := d.sign(params)
	url := fmt.Sprintf("https://api.binance.com/sapi/v1/capital/config/getall?%s&signature=%s", params, signature)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-MBX-APIKEY", d.apiKey)

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API 错误: %s", string(body))
	}

	var infos []CoinInfo
	if err := json.Unmarshal(body, &infos); err != nil {
		return nil, err
	}

	return infos, nil
}

func (d *BinanceDiscovery) sign(params string) string {
	h := hmac.New(sha256.New, []byte(d.secretKey))
	h.Write([]byte(params))
	return hex.EncodeToString(h.Sum(nil))
}

// GetPair queries PancakeSwap V2 Factory for the LP pair address of two tokens.
// Returns zero address if no pair exists.
func (d *BinanceDiscovery) GetPair(ctx context.Context, factoryAddr, tokenA, tokenB string) (string, error) {
	parsedABI, err := abi.JSON(strings.NewReader(FactoryV2ABI))
	if err != nil {
		return "", fmt.Errorf("parse factory ABI: %w", err)
	}

	factory := common.HexToAddress(factoryAddr)
	data, err := parsedABI.Pack("getPair", common.HexToAddress(tokenA), common.HexToAddress(tokenB))
	if err != nil {
		return "", fmt.Errorf("pack getPair: %w", err)
	}

	result, err := d.ethClient.CallContract(ctx, ethereum.CallMsg{To: &factory, Data: data}, nil)
	if err != nil {
		return "", fmt.Errorf("call getPair: %w", err)
	}

	var pairAddr common.Address
	if err := parsedABI.UnpackIntoInterface(&pairAddr, "getPair", result); err != nil {
		return "", fmt.Errorf("unpack getPair: %w", err)
	}

	return pairAddr.Hex(), nil
}

// GetPairTokens queries an LP pair contract for its token0 and token1 addresses.
func (d *BinanceDiscovery) GetPairTokens(ctx context.Context, pairAddr string) (token0, token1 string, err error) {
	parsedABI, err := abi.JSON(strings.NewReader(PairABI))
	if err != nil {
		return "", "", fmt.Errorf("parse pair ABI: %w", err)
	}

	pair := common.HexToAddress(pairAddr)

	// Query token0
	data0, _ := parsedABI.Pack("token0")
	res0, err := d.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pair, Data: data0}, nil)
	if err != nil {
		return "", "", fmt.Errorf("call token0: %w", err)
	}
	var addr0 common.Address
	if err := parsedABI.UnpackIntoInterface(&addr0, "token0", res0); err != nil {
		return "", "", fmt.Errorf("unpack token0: %w", err)
	}

	// Query token1
	data1, _ := parsedABI.Pack("token1")
	res1, err := d.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pair, Data: data1}, nil)
	if err != nil {
		return "", "", fmt.Errorf("call token1: %w", err)
	}
	var addr1 common.Address
	if err := parsedABI.UnpackIntoInterface(&addr1, "token1", res1); err != nil {
		return "", "", fmt.Errorf("unpack token1: %w", err)
	}

	return addr0.Hex(), addr1.Hex(), nil
}

// V3PoolState holds the initial state of a V3 pool from slot0 + liquidity queries
type V3PoolState struct {
	SqrtPriceX96 *big.Int
	Tick         int32
	Liquidity    *big.Int
}

// GetPoolV3 queries PancakeSwap V3 Factory for a pool address given two tokens and a fee tier.
func (d *BinanceDiscovery) GetPoolV3(ctx context.Context, factoryAddr, tokenA, tokenB string, fee uint32) (string, error) {
	parsedABI, err := abi.JSON(strings.NewReader(FactoryV3ABI))
	if err != nil {
		return "", fmt.Errorf("parse V3 factory ABI: %w", err)
	}

	factory := common.HexToAddress(factoryAddr)
	data, err := parsedABI.Pack("getPool", common.HexToAddress(tokenA), common.HexToAddress(tokenB), new(big.Int).SetUint64(uint64(fee)))
	if err != nil {
		return "", fmt.Errorf("pack getPool: %w", err)
	}

	result, err := d.ethClient.CallContract(ctx, ethereum.CallMsg{To: &factory, Data: data}, nil)
	if err != nil {
		return "", fmt.Errorf("call getPool: %w", err)
	}

	var poolAddr common.Address
	if err := parsedABI.UnpackIntoInterface(&poolAddr, "getPool", result); err != nil {
		return "", fmt.Errorf("unpack getPool: %w", err)
	}

	return poolAddr.Hex(), nil
}

// GetV3PoolState queries a V3 pool for its slot0 (sqrtPriceX96, tick) and liquidity.
func (d *BinanceDiscovery) GetV3PoolState(ctx context.Context, poolAddr string) (*V3PoolState, string, string, error) {
	parsedABI, err := abi.JSON(strings.NewReader(V3PoolABI))
	if err != nil {
		return nil, "", "", fmt.Errorf("parse V3 pool ABI: %w", err)
	}

	pool := common.HexToAddress(poolAddr)

	// Query slot0
	data, _ := parsedABI.Pack("slot0")
	result, err := d.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: data}, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("call slot0: %w", err)
	}

	slot0, err := parsedABI.Unpack("slot0", result)
	if err != nil || len(slot0) < 2 {
		return nil, "", "", fmt.Errorf("unpack slot0: %w", err)
	}
	sqrtPriceX96 := slot0[0].(*big.Int)
	tick := int32(slot0[1].(*big.Int).Int64())

	// Query liquidity
	data, _ = parsedABI.Pack("liquidity")
	result, err = d.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: data}, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("call liquidity: %w", err)
	}

	var liquidity *big.Int
	if err := parsedABI.UnpackIntoInterface(&liquidity, "liquidity", result); err != nil {
		return nil, "", "", fmt.Errorf("unpack liquidity: %w", err)
	}

	// Query token0/token1
	data0, _ := parsedABI.Pack("token0")
	res0, err := d.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: data0}, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("call token0: %w", err)
	}
	var addr0 common.Address
	if err := parsedABI.UnpackIntoInterface(&addr0, "token0", res0); err != nil {
		return nil, "", "", fmt.Errorf("unpack token0: %w", err)
	}

	data1, _ := parsedABI.Pack("token1")
	res1, err := d.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: data1}, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("call token1: %w", err)
	}
	var addr1 common.Address
	if err := parsedABI.UnpackIntoInterface(&addr1, "token1", res1); err != nil {
		return nil, "", "", fmt.Errorf("unpack token1: %w", err)
	}

	return &V3PoolState{
		SqrtPriceX96: sqrtPriceX96,
		Tick:         tick,
		Liquidity:    liquidity,
	}, addr0.Hex(), addr1.Hex(), nil
}

// DiscoverPoolsV3 finds V3 pool addresses for a list of tokens.
// For each token, it checks Token/USDT across all fee tiers and picks the one with highest liquidity.
func (d *BinanceDiscovery) DiscoverPoolsV3(ctx context.Context, tokens []*registry.TokenInfo, bscCfg config.BSCConfig) map[string]*PoolInfo {
	if bscCfg.FactoryV3 == "" {
		return nil
	}

	zeroAddr := common.Address{}.Hex()
	result := make(map[string]*PoolInfo)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for _, token := range tokens {
		wg.Add(1)
		go func(t *registry.TokenInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var bestPool *PoolInfo
			var bestLiquidity *big.Int

			for _, fee := range V3FeeTiers {
				poolAddr, err := d.GetPoolV3(ctx, bscCfg.FactoryV3, t.ContractAddress, bscCfg.USDT, fee)
				if err != nil {
					continue
				}
				if poolAddr == zeroAddr {
					continue
				}

				state, token0, token1, err := d.GetV3PoolState(ctx, poolAddr)
				if err != nil {
					log.Debug().Err(err).Str("symbol", t.Symbol).Uint32("fee", fee).Msg("V3 池 slot0 查询失败")
					continue
				}

				if state.Liquidity.Sign() == 0 || state.SqrtPriceX96.Sign() == 0 {
					continue
				}

				if bestLiquidity == nil || state.Liquidity.Cmp(bestLiquidity) > 0 {
					bestLiquidity = state.Liquidity
					bestPool = &PoolInfo{
						PairAddress: poolAddr,
						Token0:      token0,
						Token1:      token1,
						Version:     3,
						FeeTier:     fee,
					}
				}
			}

			if bestPool != nil {
				mu.Lock()
				result[t.Symbol] = bestPool
				mu.Unlock()
			}
		}(token)
	}

	wg.Wait()
	log.Info().Int("v3_pools", len(result)).Int("tokens", len(tokens)).Msg("V3 池发现完成")
	return result
}

// DiscoverPools finds LP pool addresses for a list of tokens.
// For each token, it tries Token/USDT pair first, then Token/WBNB.
func (d *BinanceDiscovery) DiscoverPools(ctx context.Context, tokens []*registry.TokenInfo, bscCfg config.BSCConfig) map[string]*PoolInfo {
	zeroAddr := common.Address{}.Hex()
	result := make(map[string]*PoolInfo) // key: token symbol
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10) // concurrency limit

	for _, token := range tokens {
		wg.Add(1)
		go func(t *registry.TokenInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Try Token/USDT pair first
			pairAddr, err := d.GetPair(ctx, bscCfg.FactoryV2, t.ContractAddress, bscCfg.USDT)
			if err == nil && pairAddr != zeroAddr {
				token0, token1, err := d.GetPairTokens(ctx, pairAddr)
				if err == nil {
					mu.Lock()
					result[t.Symbol] = &PoolInfo{PairAddress: pairAddr, Token0: token0, Token1: token1}
					mu.Unlock()
					return
				}
			}

			// Fallback: Token/WBNB pair
			pairAddr, err = d.GetPair(ctx, bscCfg.FactoryV2, t.ContractAddress, bscCfg.WBNB)
			if err == nil && pairAddr != zeroAddr {
				token0, token1, err := d.GetPairTokens(ctx, pairAddr)
				if err == nil {
					mu.Lock()
					result[t.Symbol] = &PoolInfo{PairAddress: pairAddr, Token0: token0, Token1: token1}
					mu.Unlock()
					return
				}
			}

			log.Debug().Str("symbol", t.Symbol).Msg("未找到LP池")
		}(token)
	}

	wg.Wait()
	log.Info().Int("pools", len(result)).Int("tokens", len(tokens)).Msg("LP 池发现完成")
	return result
}
