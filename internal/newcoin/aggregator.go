package newcoin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ──────────────────────────────────────────────────────────────────────────────
// Common Types
// ──────────────────────────────────────────────────────────────────────────────

// DEXQuote represents a quote from a DEX aggregator.
type DEXQuote struct {
	Source   string          // "1inch", "pancakeswap", "v2_direct"
	AmountIn decimal.Decimal // USDT input (human-readable)
	TokenOut decimal.Decimal // token output (human-readable)
	Price    decimal.Decimal // effective price (USDT per token)

	// For execution
	RouterAddr string // contract to call
	CallData   []byte // tx calldata
	Value      *big.Int
	Gas        uint64
}

// DEXAggregator fetches quotes from multiple DEX sources and picks the best.
type DEXAggregator struct {
	oneInch  *OneInchClient
	pancake  *PancakeQuoterClient
	usdtAddr string
	chainID  int
}

// NewDEXAggregator creates an aggregator with 1inch and PancakeSwap support.
func NewDEXAggregator(ethClient *ethclient.Client, usdtAddr string) *DEXAggregator {
	oneInchKey := os.Getenv("ONEINCH_API_KEY")

	agg := &DEXAggregator{
		usdtAddr: usdtAddr,
		chainID:  56, // BSC
	}

	if oneInchKey != "" {
		agg.oneInch = NewOneInchClient(oneInchKey, 56)
		log.Info().Msg("DEX Aggregator: 1inch enabled")
	} else {
		log.Warn().Msg("DEX Aggregator: 1inch disabled (set ONEINCH_API_KEY)")
	}

	agg.pancake = NewPancakeQuoterClient(ethClient)
	log.Info().Msg("DEX Aggregator: PancakeSwap QuoterV2 enabled")

	return agg
}

// GetBestQuote gets quotes from all sources and returns the best one (most tokens out).
func (a *DEXAggregator) GetBestQuote(tokenAddr string, decimals int, amountUSD float64, buyToken bool) (*DEXQuote, error) {
	amountWei := decimal.NewFromFloat(amountUSD).Mul(decimal.New(1, 18)).BigInt()

	var srcToken, dstToken string
	if buyToken {
		srcToken = a.usdtAddr
		dstToken = tokenAddr
	} else {
		srcToken = tokenAddr
		dstToken = a.usdtAddr
	}

	type result struct {
		quote *DEXQuote
		err   error
		src   string
	}

	ch := make(chan result, 2)

	// 1inch quote (parallel)
	if a.oneInch != nil {
		go func() {
			q, err := a.oneInch.Quote(srcToken, dstToken, amountWei)
			if err != nil {
				log.Debug().Err(err).Msg("1inch quote failed")
			}
			ch <- result{quote: q, err: err, src: "1inch"}
		}()
	} else {
		ch <- result{src: "1inch", err: errors.New("not configured")}
	}

	// PancakeSwap quote (parallel)
	go func() {
		q, err := a.pancake.Quote(srcToken, dstToken, decimals, amountWei, buyToken)
		if err != nil {
			log.Debug().Err(err).Msg("PancakeSwap quote failed")
		}
		ch <- result{quote: q, err: err, src: "pancakeswap"}
	}()

	var best *DEXQuote
	var lastErr error

	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			lastErr = r.err
			continue
		}
		if r.quote == nil {
			continue
		}

		if buyToken {
			// Buying tokens: want max tokens out
			if best == nil || r.quote.TokenOut.GreaterThan(best.TokenOut) {
				if best != nil {
					log.Info().
						Str("winner", r.quote.Source).
						Str("winner_out", r.quote.TokenOut.StringFixed(4)).
						Str("loser", best.Source).
						Str("loser_out", best.TokenOut.StringFixed(4)).
						Msg("Aggregator: better quote found")
				}
				best = r.quote
			}
		} else {
			// Selling tokens: want max USDT out (TokenOut is USDT here)
			if best == nil || r.quote.TokenOut.GreaterThan(best.TokenOut) {
				best = r.quote
			}
		}
	}

	if best != nil {
		log.Info().
			Str("source", best.Source).
			Str("amount_in", best.AmountIn.StringFixed(2)).
			Str("token_out", best.TokenOut.StringFixed(6)).
			Str("price", best.Price.StringFixed(8)).
			Msg("Aggregator: best quote")
		return best, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all quotes failed: %w", lastErr)
	}
	return nil, errors.New("no quotes available")
}

// GetBestSwap gets swap calldata from all sources and returns the best.
func (a *DEXAggregator) GetBestSwap(tokenAddr string, decimals int, amountUSD float64, buyToken bool, fromAddr string, slippage float64) (*DEXQuote, error) {
	amountWei := decimal.NewFromFloat(amountUSD).Mul(decimal.New(1, 18)).BigInt()

	var srcToken, dstToken string
	if buyToken {
		srcToken = a.usdtAddr
		dstToken = tokenAddr
	} else {
		srcToken = tokenAddr
		dstToken = a.usdtAddr
	}

	type result struct {
		quote *DEXQuote
		err   error
		src   string
	}
	ch := make(chan result, 2)

	// 1inch swap (parallel)
	if a.oneInch != nil {
		go func() {
			q, err := a.oneInch.Swap(srcToken, dstToken, amountWei, fromAddr, slippage)
			if err != nil {
				log.Debug().Err(err).Msg("1inch swap failed")
			}
			ch <- result{quote: q, err: err, src: "1inch"}
		}()
	} else {
		ch <- result{src: "1inch", err: errors.New("not configured")}
	}

	// PancakeSwap swap (we build calldata ourselves from smart router)
	go func() {
		q, err := a.pancake.Swap(srcToken, dstToken, decimals, amountWei, buyToken, fromAddr, slippage)
		if err != nil {
			log.Debug().Err(err).Msg("PancakeSwap swap failed")
		}
		ch <- result{quote: q, err: err, src: "pancakeswap"}
	}()

	var best *DEXQuote
	var lastErr error

	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			lastErr = r.err
			continue
		}
		if r.quote == nil {
			continue
		}
		if buyToken {
			if best == nil || r.quote.TokenOut.GreaterThan(best.TokenOut) {
				best = r.quote
			}
		} else {
			if best == nil || r.quote.TokenOut.GreaterThan(best.TokenOut) {
				best = r.quote
			}
		}
	}

	if best != nil {
		log.Info().
			Str("source", best.Source).
			Str("token_out", best.TokenOut.StringFixed(6)).
			Str("price", best.Price.StringFixed(8)).
			Str("router", best.RouterAddr).
			Msg("Aggregator: best swap selected")
		return best, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("all swaps failed: %w", lastErr)
	}
	return nil, errors.New("no swap available")
}

// GetQuotePrice returns the best aggregated price (USDT per token) for a given amount.
// This is used for accurate price comparison with CEX.
func (a *DEXAggregator) GetQuotePrice(tokenAddr string, decimals int, amountUSD float64) decimal.Decimal {
	q, err := a.GetBestQuote(tokenAddr, decimals, amountUSD, true)
	if err != nil {
		return decimal.Zero
	}
	return q.Price
}

// ──────────────────────────────────────────────────────────────────────────────
// 1inch Swap API v6.0
// ──────────────────────────────────────────────────────────────────────────────

type OneInchClient struct {
	apiKey     string
	chainID    int
	httpClient *http.Client
	baseURL    string
}

func NewOneInchClient(apiKey string, chainID int) *OneInchClient {
	return &OneInchClient{
		apiKey:     apiKey,
		chainID:    chainID,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    fmt.Sprintf("https://api.1inch.dev/swap/v6.0/%d", chainID),
	}
}

// 1inch quote response
type oneInchQuoteResp struct {
	DstAmount string `json:"dstAmount"`
	Gas       int64  `json:"gas"`
}

// 1inch swap response
type oneInchSwapResp struct {
	DstAmount string `json:"dstAmount"`
	Tx        struct {
		To       string `json:"to"`
		Data     string `json:"data"`
		Value    string `json:"value"`
		Gas      int64  `json:"gas"`
		GasPrice string `json:"gasPrice"`
	} `json:"tx"`
}

func (c *OneInchClient) Quote(srcToken, dstToken string, amountWei *big.Int) (*DEXQuote, error) {
	url := fmt.Sprintf("%s/quote?src=%s&dst=%s&amount=%s",
		c.baseURL, srcToken, dstToken, amountWei.String())

	body, err := c.doRequest(url)
	if err != nil {
		return nil, err
	}

	var resp oneInchQuoteResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("1inch: unmarshal error: %w (body: %s)", err, string(body))
	}

	dstAmount, ok := new(big.Int).SetString(resp.DstAmount, 10)
	if !ok {
		return nil, fmt.Errorf("1inch: invalid dstAmount: %s", resp.DstAmount)
	}

	// We don't know dst decimals yet, return raw for now
	// Caller will interpret based on context
	amountIn := decimal.NewFromBigInt(amountWei, -18) // USDT always 18 on BSC
	tokenOut := decimal.NewFromBigInt(dstAmount, 0)   // raw, needs decimal adjustment

	var price decimal.Decimal
	if tokenOut.IsPositive() {
		price = amountIn.Div(tokenOut)
	}

	return &DEXQuote{
		Source:   "1inch",
		AmountIn: amountIn,
		TokenOut: tokenOut,
		Price:    price,
		Gas:      uint64(resp.Gas),
	}, nil
}

func (c *OneInchClient) QuoteWithDecimals(srcToken, dstToken string, amountWei *big.Int, dstDecimals int) (*DEXQuote, error) {
	q, err := c.Quote(srcToken, dstToken, amountWei)
	if err != nil {
		return nil, err
	}

	// Now adjust tokenOut for decimals
	q.TokenOut = q.TokenOut.Div(decimal.New(1, int32(dstDecimals)))
	if q.TokenOut.IsPositive() {
		q.Price = q.AmountIn.Div(q.TokenOut)
	}
	return q, nil
}

func (c *OneInchClient) Swap(srcToken, dstToken string, amountWei *big.Int, fromAddr string, slippage float64) (*DEXQuote, error) {
	url := fmt.Sprintf("%s/swap?src=%s&dst=%s&amount=%s&from=%s&slippage=%.1f&disableEstimate=true",
		c.baseURL, srcToken, dstToken, amountWei.String(), fromAddr, slippage)

	body, err := c.doRequest(url)
	if err != nil {
		return nil, err
	}

	var resp oneInchSwapResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("1inch: unmarshal swap error: %w (body: %s)", err, string(body))
	}

	dstAmount, ok := new(big.Int).SetString(resp.DstAmount, 10)
	if !ok {
		return nil, fmt.Errorf("1inch: invalid dstAmount: %s", resp.DstAmount)
	}

	callData := common.FromHex(resp.Tx.Data)

	txValue := big.NewInt(0)
	if resp.Tx.Value != "" && resp.Tx.Value != "0" {
		txValue, _ = new(big.Int).SetString(resp.Tx.Value, 10)
	}

	amountIn := decimal.NewFromBigInt(amountWei, -18)
	tokenOut := decimal.NewFromBigInt(dstAmount, 0) // raw, caller adjusts decimals

	var price decimal.Decimal
	if tokenOut.IsPositive() {
		price = amountIn.Div(tokenOut)
	}

	return &DEXQuote{
		Source:     "1inch",
		AmountIn:   amountIn,
		TokenOut:   tokenOut,
		Price:      price,
		RouterAddr: resp.Tx.To,
		CallData:   callData,
		Value:      txValue,
		Gas:        uint64(resp.Tx.Gas),
	}, nil
}

func (c *OneInchClient) doRequest(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("1inch: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("1inch: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// PancakeSwap QuoterV2 + SmartRouter (on-chain)
// ──────────────────────────────────────────────────────────────────────────────

// PancakeSwap V3 contracts on BSC
const (
	pancakeQuoterV2Addr    = "0xB048Bbc1Ee6b733FFfCFb9e9CeF7375518e25997"
	pancakeSmartRouterAddr = "0x13f4EA83D0bd40E75C8222255bc855a974568Dd4"
)

// V3 fee tiers to try: 100 (0.01%), 500 (0.05%), 2500 (0.25%), 10000 (1%)
var pancakeV3FeeTiers = []uint32{100, 500, 2500, 10000}

// QuoterV2 ABI (quoteExactInputSingle)
const quoterV2ABI = `[
	{
		"inputs": [
			{
				"components": [
					{"internalType": "address", "name": "tokenIn", "type": "address"},
					{"internalType": "address", "name": "tokenOut", "type": "address"},
					{"internalType": "uint256", "name": "amountIn", "type": "uint256"},
					{"internalType": "uint24", "name": "fee", "type": "uint24"},
					{"internalType": "uint160", "name": "sqrtPriceLimitX96", "type": "uint160"}
				],
				"internalType": "struct IQuoterV2.QuoteExactInputSingleParams",
				"name": "params",
				"type": "tuple"
			}
		],
		"name": "quoteExactInputSingle",
		"outputs": [
			{"internalType": "uint256", "name": "amountOut", "type": "uint256"},
			{"internalType": "uint160", "name": "sqrtPriceX96After", "type": "uint160"},
			{"internalType": "uint32", "name": "initializedTicksCrossed", "type": "uint32"},
			{"internalType": "uint256", "name": "gasEstimate", "type": "uint256"}
		],
		"stateMutability": "nonpayable",
		"type": "function"
	}
]`

// SmartRouter V3 ABI (exactInputSingle)
const smartRouterV3ABI = `[
	{
		"inputs": [
			{
				"components": [
					{"internalType": "address", "name": "tokenIn", "type": "address"},
					{"internalType": "address", "name": "tokenOut", "type": "address"},
					{"internalType": "uint24",  "name": "fee", "type": "uint24"},
					{"internalType": "address", "name": "recipient", "type": "address"},
					{"internalType": "uint256", "name": "amountIn", "type": "uint256"},
					{"internalType": "uint256", "name": "amountOutMinimum", "type": "uint256"},
					{"internalType": "uint160", "name": "sqrtPriceLimitX96", "type": "uint160"}
				],
				"internalType": "struct IV3SwapRouter.ExactInputSingleParams",
				"name": "params",
				"type": "tuple"
			}
		],
		"name": "exactInputSingle",
		"outputs": [
			{"internalType": "uint256", "name": "amountOut", "type": "uint256"}
		],
		"stateMutability": "payable",
		"type": "function"
	}
]`

type PancakeQuoterClient struct {
	ethClient  *ethclient.Client
	quoterABI  abi.ABI
	routerABI  abi.ABI
	quoterAddr common.Address
	routerAddr common.Address
}

func NewPancakeQuoterClient(ethClient *ethclient.Client) *PancakeQuoterClient {
	qABI, _ := abi.JSON(strings.NewReader(quoterV2ABI))
	rABI, _ := abi.JSON(strings.NewReader(smartRouterV3ABI))

	return &PancakeQuoterClient{
		ethClient:  ethClient,
		quoterABI:  qABI,
		routerABI:  rABI,
		quoterAddr: common.HexToAddress(pancakeQuoterV2Addr),
		routerAddr: common.HexToAddress(pancakeSmartRouterAddr),
	}
}

// Quote gets the best PancakeSwap V3 quote across all fee tiers.
func (c *PancakeQuoterClient) Quote(srcToken, dstToken string, dstDecimals int, amountWei *big.Int, buyToken bool) (*DEXQuote, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var bestAmountOut *big.Int
	var bestFee uint32
	var bestGas uint64

	srcAddr := common.HexToAddress(srcToken)
	dstAddr := common.HexToAddress(dstToken)

	for _, fee := range pancakeV3FeeTiers {
		// Build quoteExactInputSingle params
		type QuoteParams struct {
			TokenIn           common.Address
			TokenOut          common.Address
			AmountIn          *big.Int
			Fee               *big.Int
			SqrtPriceLimitX96 *big.Int
		}

		params := QuoteParams{
			TokenIn:           srcAddr,
			TokenOut:          dstAddr,
			AmountIn:          amountWei,
			Fee:               big.NewInt(int64(fee)),
			SqrtPriceLimitX96: big.NewInt(0),
		}

		data, err := c.quoterABI.Pack("quoteExactInputSingle", params)
		if err != nil {
			log.Trace().Err(err).Uint32("fee", fee).Msg("PCS: pack error")
			continue
		}

		result, err := c.ethClient.CallContract(ctx, ethereum.CallMsg{
			To:   &c.quoterAddr,
			Data: data,
		}, nil)
		if err != nil {
			log.Trace().Err(err).Uint32("fee", fee).Msg("PCS: quote call failed")
			continue
		}

		// Unpack: (uint256 amountOut, uint160 sqrtPriceX96After, uint32 initializedTicksCrossed, uint256 gasEstimate)
		outputs, err := c.quoterABI.Unpack("quoteExactInputSingle", result)
		if err != nil || len(outputs) < 4 {
			continue
		}

		amountOut := outputs[0].(*big.Int)
		gasEst := outputs[3].(*big.Int)

		if amountOut.Sign() > 0 && (bestAmountOut == nil || amountOut.Cmp(bestAmountOut) > 0) {
			bestAmountOut = amountOut
			bestFee = fee
			bestGas = gasEst.Uint64()
		}
	}

	if bestAmountOut == nil {
		return nil, errors.New("PCS: no V3 pool has liquidity for this pair")
	}

	amountIn := decimal.NewFromBigInt(amountWei, -18) // USDT 18 decimals

	var tokenOut decimal.Decimal
	if buyToken {
		// Output is tokens with dstDecimals
		tokenOut = decimal.NewFromBigInt(bestAmountOut, -int32(dstDecimals))
	} else {
		// Output is USDT (18 decimals)
		tokenOut = decimal.NewFromBigInt(bestAmountOut, -18)
	}

	var price decimal.Decimal
	if tokenOut.IsPositive() {
		if buyToken {
			price = amountIn.Div(tokenOut)
		} else {
			price = tokenOut.Div(amountIn)
		}
	}

	log.Debug().
		Str("src", srcToken[:10]+"...").
		Str("dst", dstToken[:10]+"...").
		Uint32("best_fee", bestFee).
		Str("amount_out", tokenOut.StringFixed(6)).
		Str("price", price.StringFixed(8)).
		Msg("PCS V3 best quote")

	return &DEXQuote{
		Source:   fmt.Sprintf("pancakeswap_v3_%d", bestFee),
		AmountIn: amountIn,
		TokenOut: tokenOut,
		Price:    price,
		Gas:      bestGas,
	}, nil
}

// Swap builds the PancakeSwap SmartRouter V3 calldata for the best fee tier.
func (c *PancakeQuoterClient) Swap(srcToken, dstToken string, dstDecimals int, amountWei *big.Int, buyToken bool, fromAddr string, slippage float64) (*DEXQuote, error) {
	// First get the best quote to find optimal fee tier
	q, err := c.Quote(srcToken, dstToken, dstDecimals, amountWei, buyToken)
	if err != nil {
		return nil, err
	}

	// Parse fee tier from source string
	var feeTier uint32
	fmt.Sscanf(q.Source, "pancakeswap_v3_%d", &feeTier)
	if feeTier == 0 {
		feeTier = 2500 // default
	}

	// Calculate amountOutMin with slippage
	var amountOutRaw *big.Int
	if buyToken {
		amountOutRaw = q.TokenOut.Mul(decimal.New(1, int32(dstDecimals))).BigInt()
	} else {
		amountOutRaw = q.TokenOut.Mul(decimal.New(1, 18)).BigInt()
	}

	slippageFactor := decimal.NewFromFloat(100 - slippage).Div(decimal.NewFromInt(100))
	amountOutMin := decimal.NewFromBigInt(amountOutRaw, 0).Mul(slippageFactor).BigInt()

	// Build exactInputSingle calldata
	type ExactInputSingleParams struct {
		TokenIn           common.Address
		TokenOut          common.Address
		Fee               *big.Int
		Recipient         common.Address
		AmountIn          *big.Int
		AmountOutMinimum  *big.Int
		SqrtPriceLimitX96 *big.Int
	}

	params := ExactInputSingleParams{
		TokenIn:           common.HexToAddress(srcToken),
		TokenOut:          common.HexToAddress(dstToken),
		Fee:               big.NewInt(int64(feeTier)),
		Recipient:         common.HexToAddress(fromAddr),
		AmountIn:          amountWei,
		AmountOutMinimum:  amountOutMin,
		SqrtPriceLimitX96: big.NewInt(0),
	}

	callData, err := c.routerABI.Pack("exactInputSingle", params)
	if err != nil {
		return nil, fmt.Errorf("PCS: pack swap failed: %w", err)
	}

	q.RouterAddr = pancakeSmartRouterAddr
	q.CallData = callData
	q.Value = big.NewInt(0)

	return q, nil
}
