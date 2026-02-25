package newcoin

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
	"strings"
	"sync"
	"time"

	"ocdex/config"
	"ocdex/internal/discovery"
	"ocdex/internal/registry"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// CoinListItem represents a Binance coin with BSC support and market data.
type CoinListItem struct {
	Symbol          string `json:"symbol"`
	Name            string `json:"name"`
	CEXSymbol       string `json:"cex_symbol"`
	ContractAddress string `json:"contract_address"`
	Price           string `json:"price"`
	Volume24h       string `json:"volume_24h"`
	PriceChangePct  string `json:"price_change_pct"`
	WithdrawEnabled bool   `json:"withdraw_enabled"`
	DepositEnabled  bool   `json:"deposit_enabled"`
	CEXMultiplier   int64  `json:"cex_multiplier"`
}

// PoolDiscoveryResult represents a discovered DEX pool.
type PoolDiscoveryResult struct {
	Version    int    `json:"version"`
	Address    string `json:"address"`
	Token0     string `json:"token0"`
	Token1     string `json:"token1"`
	FeeTier    uint32 `json:"fee_tier"`
	QuoteToken string `json:"quote_token"` // "USDT" or "WBNB"
}

// BinanceAPI handles Binance HTTP API calls and on-chain pool discovery.
type BinanceAPI struct {
	apiKey     string
	secretKey  string
	httpClient *http.Client
	ethClient  *ethclient.Client
	bscCfg     config.BSCConfig

	cacheMu sync.RWMutex
	cache   []CoinListItem
	cacheAt time.Time
}

// NewBinanceAPI creates a new BinanceAPI instance.
func NewBinanceAPI(apiCfg config.APIConfig, ethClient *ethclient.Client, bscCfg config.BSCConfig) *BinanceAPI {
	return &BinanceAPI{
		apiKey:     apiCfg.ApiKey,
		secretKey:  apiCfg.SecretKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		ethClient:  ethClient,
		bscCfg:     bscCfg,
	}
}

// --- Public API ---

// FetchBSCCoins returns BSC-enabled coins with 24h market data. Results are cached for 5 minutes.
func (b *BinanceAPI) FetchBSCCoins(ctx context.Context) ([]CoinListItem, error) {
	b.cacheMu.RLock()
	if len(b.cache) > 0 && time.Since(b.cacheAt) < 5*time.Minute {
		result := make([]CoinListItem, len(b.cache))
		copy(result, b.cache)
		b.cacheMu.RUnlock()
		return result, nil
	}
	b.cacheMu.RUnlock()

	items, err := b.fetchCoinsFromAPI(ctx)
	if err != nil {
		return nil, err
	}

	b.cacheMu.Lock()
	b.cache = items
	b.cacheAt = time.Now()
	b.cacheMu.Unlock()

	return items, nil
}

// DiscoverPools finds V2 and V3 pools for a token against both USDT and WBNB.
func (b *BinanceAPI) DiscoverPools(ctx context.Context, contractAddress string) ([]PoolDiscoveryResult, error) {
	var results []PoolDiscoveryResult

	// USDT direct pools
	usdtPools := b.findPools(ctx, contractAddress, b.bscCfg.USDT, "USDT")
	results = append(results, usdtPools...)

	// WBNB pools (skip if token IS WBNB)
	if b.bscCfg.WBNB != "" && !strings.EqualFold(contractAddress, b.bscCfg.WBNB) {
		wbnbPools := b.findPools(ctx, contractAddress, b.bscCfg.WBNB, "WBNB")
		results = append(results, wbnbPools...)
	}

	return results, nil
}

// DiscoverWBNBUSDTPools discovers WBNB/USDT pools (needed for WBNB price conversion).
func (b *BinanceAPI) DiscoverWBNBUSDTPools(ctx context.Context) []PoolDiscoveryResult {
	if b.bscCfg.WBNB == "" {
		return nil
	}
	return b.findPools(ctx, b.bscCfg.WBNB, b.bscCfg.USDT, "USDT")
}

// findPools discovers V2 + V3 pools for tokenAddr/quoteAddr pair.
func (b *BinanceAPI) findPools(ctx context.Context, tokenAddr, quoteAddr, quoteLabel string) []PoolDiscoveryResult {
	var results []PoolDiscoveryResult
	zeroAddr := common.Address{}.Hex()

	// V2
	if b.bscCfg.FactoryV2 != "" {
		pairAddr, err := b.callGetPair(ctx, b.bscCfg.FactoryV2, tokenAddr, quoteAddr)
		if err == nil && pairAddr != zeroAddr {
			token0, token1, err := b.callGetPairTokens(ctx, pairAddr)
			if err == nil {
				results = append(results, PoolDiscoveryResult{
					Version: 2, Address: pairAddr,
					Token0: token0, Token1: token1,
					QuoteToken: quoteLabel,
				})
			}
		}
	}

	// V3 all fee tiers
	if b.bscCfg.FactoryV3 != "" {
		for _, fee := range discovery.V3FeeTiers {
			poolAddr, err := b.callGetPoolV3(ctx, b.bscCfg.FactoryV3, tokenAddr, quoteAddr, fee)
			if err != nil || poolAddr == zeroAddr {
				continue
			}
			token0, token1, err := b.callGetPoolTokens(ctx, poolAddr)
			if err != nil {
				continue
			}
			results = append(results, PoolDiscoveryResult{
				Version: 3, Address: poolAddr,
				Token0: token0, Token1: token1,
				FeeTier: fee, QuoteToken: quoteLabel,
			})
		}
	}

	return results
}

// FetchDecimals queries on-chain decimals for a token contract.
func (b *BinanceAPI) FetchDecimals(ctx context.Context, contractAddress string) (int, error) {
	callData := common.Hex2Bytes("313ce567") // decimals()
	addr := common.HexToAddress(contractAddress)
	result, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: callData}, nil)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, fmt.Errorf("empty result")
	}
	return int(new(big.Int).SetBytes(result).Int64()), nil
}

// --- Private: Binance HTTP API ---

type exchangeInfoResp struct {
	Symbols []symbolInfoResp `json:"symbols"`
}

type symbolInfoResp struct {
	Symbol     string `json:"symbol"`
	BaseAsset  string `json:"baseAsset"`
	QuoteAsset string `json:"quoteAsset"`
	Status     string `json:"status"`
}

type coinInfoResp struct {
	Coin        string            `json:"coin"`
	Name        string            `json:"name"`
	NetworkList []networkInfoResp `json:"networkList"`
}

type networkInfoResp struct {
	Network         string `json:"network"`
	ContractAddress string `json:"contractAddress"`
	WithdrawEnable  bool   `json:"withdrawEnable"`
	DepositEnable   bool   `json:"depositEnable"`
}

type ticker24hResp struct {
	Symbol             string `json:"symbol"`
	LastPrice          string `json:"lastPrice"`
	QuoteVolume        string `json:"quoteVolume"`
	PriceChangePercent string `json:"priceChangePercent"`
}

func (b *BinanceAPI) fetchCoinsFromAPI(ctx context.Context) ([]CoinListItem, error) {
	type fetchResult struct {
		kind    string
		symbols map[string]bool
		coins   []coinInfoResp
		tickers map[string]ticker24hResp
		err     error
	}

	ch := make(chan fetchResult, 3)

	go func() {
		s, err := b.getUSDTSymbols(ctx)
		ch <- fetchResult{kind: "symbols", symbols: s, err: err}
	}()
	go func() {
		c, err := b.getCoinInfos(ctx)
		ch <- fetchResult{kind: "coins", coins: c, err: err}
	}()
	go func() {
		t, err := b.get24hTickers(ctx)
		ch <- fetchResult{kind: "tickers", tickers: t, err: err}
	}()

	var symbols map[string]bool
	var coins []coinInfoResp
	var tickers map[string]ticker24hResp

	for i := 0; i < 3; i++ {
		r := <-ch
		if r.err != nil {
			if r.kind == "tickers" {
				// Tickers are non-critical
				tickers = make(map[string]ticker24hResp)
				continue
			}
			return nil, fmt.Errorf("%s: %w", r.kind, r.err)
		}
		switch r.kind {
		case "symbols":
			symbols = r.symbols
		case "coins":
			coins = r.coins
		case "tickers":
			tickers = r.tickers
		}
	}

	stablecoins := map[string]bool{
		"USDC": true, "BUSD": true, "DAI": true, "TUSD": true,
		"USDP": true, "FDUSD": true, "USDD": true, "PYUSD": true,
	}

	var items []CoinListItem
	for _, coin := range coins {
		if stablecoins[coin.Coin] {
			continue
		}
		cexSymbol := coin.Coin + "USDT"
		if !symbols[cexSymbol] {
			continue
		}

		var bscNet *networkInfoResp
		for i := range coin.NetworkList {
			if coin.NetworkList[i].Network == "BSC" && coin.NetworkList[i].ContractAddress != "" {
				bscNet = &coin.NetworkList[i]
				break
			}
		}
		if bscNet == nil {
			continue
		}

		item := CoinListItem{
			Symbol:          coin.Coin,
			Name:            coin.Name,
			CEXSymbol:       cexSymbol,
			ContractAddress: bscNet.ContractAddress,
			WithdrawEnabled: bscNet.WithdrawEnable,
			DepositEnabled:  bscNet.DepositEnable,
			CEXMultiplier:   registry.ParseCEXMultiplier(coin.Coin),
		}

		if t, ok := tickers[cexSymbol]; ok {
			item.Price = t.LastPrice
			item.Volume24h = t.QuoteVolume
			item.PriceChangePct = t.PriceChangePercent
		}

		items = append(items, item)
	}

	return items, nil
}

func (b *BinanceAPI) getUSDTSymbols(ctx context.Context) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.binance.com/api/v3/exchangeInfo", nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var info exchangeInfoResp
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

func (b *BinanceAPI) getCoinInfos(ctx context.Context) ([]coinInfoResp, error) {
	if b.apiKey == "" || b.secretKey == "" {
		return nil, fmt.Errorf("API key not configured")
	}

	timestamp := time.Now().UnixMilli()
	params := fmt.Sprintf("timestamp=%d", timestamp)
	sig := b.sign(params)
	url := fmt.Sprintf("https://api.binance.com/sapi/v1/capital/config/getall?%s&signature=%s", params, sig)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", b.apiKey)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var infos []coinInfoResp
	if err := json.Unmarshal(body, &infos); err != nil {
		return nil, err
	}
	return infos, nil
}

func (b *BinanceAPI) get24hTickers(ctx context.Context) (map[string]ticker24hResp, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.binance.com/api/v3/ticker/24hr", nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var tickers []ticker24hResp
	if err := json.Unmarshal(body, &tickers); err != nil {
		return nil, err
	}

	result := make(map[string]ticker24hResp, len(tickers))
	for _, t := range tickers {
		if strings.HasSuffix(t.Symbol, "USDT") {
			result[t.Symbol] = t
		}
	}
	return result, nil
}

func (b *BinanceAPI) sign(params string) string {
	h := hmac.New(sha256.New, []byte(b.secretKey))
	h.Write([]byte(params))
	return hex.EncodeToString(h.Sum(nil))
}

// --- Private: On-chain calls ---

func (b *BinanceAPI) callGetPair(ctx context.Context, factoryAddr, tokenA, tokenB string) (string, error) {
	parsedABI, err := abi.JSON(strings.NewReader(discovery.FactoryV2ABI))
	if err != nil {
		return "", err
	}
	factory := common.HexToAddress(factoryAddr)
	data, err := parsedABI.Pack("getPair", common.HexToAddress(tokenA), common.HexToAddress(tokenB))
	if err != nil {
		return "", err
	}
	result, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &factory, Data: data}, nil)
	if err != nil {
		return "", err
	}
	var pairAddr common.Address
	if err := parsedABI.UnpackIntoInterface(&pairAddr, "getPair", result); err != nil {
		return "", err
	}
	return pairAddr.Hex(), nil
}

func (b *BinanceAPI) callGetPoolV3(ctx context.Context, factoryAddr, tokenA, tokenB string, fee uint32) (string, error) {
	parsedABI, err := abi.JSON(strings.NewReader(discovery.FactoryV3ABI))
	if err != nil {
		return "", err
	}
	factory := common.HexToAddress(factoryAddr)
	data, err := parsedABI.Pack("getPool",
		common.HexToAddress(tokenA),
		common.HexToAddress(tokenB),
		new(big.Int).SetUint64(uint64(fee)),
	)
	if err != nil {
		return "", err
	}
	result, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &factory, Data: data}, nil)
	if err != nil {
		return "", err
	}
	var poolAddr common.Address
	if err := parsedABI.UnpackIntoInterface(&poolAddr, "getPool", result); err != nil {
		return "", err
	}
	return poolAddr.Hex(), nil
}

func (b *BinanceAPI) callGetPairTokens(ctx context.Context, pairAddr string) (string, string, error) {
	parsedABI, err := abi.JSON(strings.NewReader(discovery.PairABI))
	if err != nil {
		return "", "", err
	}
	pair := common.HexToAddress(pairAddr)

	data0, _ := parsedABI.Pack("token0")
	res0, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pair, Data: data0}, nil)
	if err != nil {
		return "", "", err
	}
	var addr0 common.Address
	if err := parsedABI.UnpackIntoInterface(&addr0, "token0", res0); err != nil {
		return "", "", err
	}

	data1, _ := parsedABI.Pack("token1")
	res1, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pair, Data: data1}, nil)
	if err != nil {
		return "", "", err
	}
	var addr1 common.Address
	if err := parsedABI.UnpackIntoInterface(&addr1, "token1", res1); err != nil {
		return "", "", err
	}

	return addr0.Hex(), addr1.Hex(), nil
}

func (b *BinanceAPI) callGetPoolTokens(ctx context.Context, poolAddr string) (string, string, error) {
	parsedABI, err := abi.JSON(strings.NewReader(discovery.V3PoolABI))
	if err != nil {
		return "", "", err
	}
	pool := common.HexToAddress(poolAddr)

	data0, _ := parsedABI.Pack("token0")
	res0, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: data0}, nil)
	if err != nil {
		return "", "", err
	}
	var addr0 common.Address
	if err := parsedABI.UnpackIntoInterface(&addr0, "token0", res0); err != nil {
		return "", "", err
	}

	data1, _ := parsedABI.Pack("token1")
	res1, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: data1}, nil)
	if err != nil {
		return "", "", err
	}
	var addr1 common.Address
	if err := parsedABI.UnpackIntoInterface(&addr1, "token1", res1); err != nil {
		return "", "", err
	}

	return addr0.Hex(), addr1.Hex(), nil
}

// FetchV2Reserves queries V2 pool reserves.
func (b *BinanceAPI) FetchV2Reserves(ctx context.Context, poolAddr string) (*big.Int, *big.Int, error) {
	abiJSON := `[{"constant":true,"inputs":[],"name":"getReserves","outputs":[{"internalType":"uint112","name":"_reserve0","type":"uint112"},{"internalType":"uint112","name":"_reserve1","type":"uint112"},{"internalType":"uint32","name":"_blockTimestampLast","type":"uint32"}],"payable":false,"stateMutability":"view","type":"function"}]`
	parsedABI, _ := abi.JSON(strings.NewReader(abiJSON))

	pool := common.HexToAddress(poolAddr)
	data, _ := parsedABI.Pack("getReserves")

	res, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: data}, nil)
	if err != nil {
		return nil, nil, err
	}

	type Reserves struct {
		Reserve0           *big.Int
		Reserve1           *big.Int
		BlockTimestampLast uint32
	}
	var out Reserves
	if err := parsedABI.UnpackIntoInterface(&out, "getReserves", res); err != nil {
		return nil, nil, err
	}
	return out.Reserve0, out.Reserve1, nil
}

// V3State holds the state of a V3 pool.
type V3State struct {
	SqrtPriceX96 *big.Int
	Tick         int32
	Liquidity    *big.Int
}

// FetchV3State queries V3 pool state (slot0 + liquidity).
func (b *BinanceAPI) FetchV3State(ctx context.Context, poolAddr string) (*V3State, error) {
	parsedABI, _ := abi.JSON(strings.NewReader(discovery.V3PoolABI))
	pool := common.HexToAddress(poolAddr)

	// slot0
	dataSlot0, _ := parsedABI.Pack("slot0")
	resSlot0, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: dataSlot0}, nil)
	if err != nil {
		return nil, fmt.Errorf("slot0: %w", err)
	}
	slot0, err := parsedABI.Unpack("slot0", resSlot0)
	if err != nil || len(slot0) < 2 {
		return nil, fmt.Errorf("unpack slot0: %w", err)
	}

	// liquidity
	dataLiq, _ := parsedABI.Pack("liquidity")
	resLiq, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: dataLiq}, nil)
	if err != nil {
		return nil, fmt.Errorf("liquidity: %w", err)
	}
	var liquidity *big.Int
	if err := parsedABI.UnpackIntoInterface(&liquidity, "liquidity", resLiq); err != nil {
		return nil, fmt.Errorf("unpack liquidity: %w", err)
	}

	return &V3State{
		SqrtPriceX96: slot0[0].(*big.Int),
		Tick:         int32(slot0[1].(*big.Int).Int64()),
		Liquidity:    liquidity,
	}, nil
}
