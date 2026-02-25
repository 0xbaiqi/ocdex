package cex

import (
	"context"
	"fmt"
	"strconv"

	"ocdex/config"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/adshao/go-binance/v2/portfolio"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

type BinanceFuturesClient struct {
	futuresClient   *futures.Client   // 市场数据 (exchangeInfo, premiumIndex)
	portfolioClient *portfolio.Client // 交易 + 账户 (统一账户 /papi/)
	cfg             config.APIConfig
}

func NewBinanceFuturesClient(cfg config.APIConfig) *BinanceFuturesClient {
	return &BinanceFuturesClient{
		futuresClient:   binance.NewFuturesClient(cfg.ApiKey, cfg.SecretKey),
		portfolioClient: portfolio.NewClient(cfg.ApiKey, cfg.SecretKey),
		cfg:             cfg,
	}
}

// CheckFuturesPermission checks connectivity via /papi/v1/balance.
func (b *BinanceFuturesClient) CheckFuturesPermission() error {
	_, err := b.portfolioClient.NewGetBalanceService().Do(context.Background())
	if err == nil {
		return nil
	}

	// /papi/ 失败，尝试用公开接口验证网络连通性
	_, err2 := b.futuresClient.NewExchangeInfoService().Do(context.Background())
	if err2 != nil {
		return fmt.Errorf("futures API 不可达: %w", err)
	}

	// 公开接口 OK，说明是权限问题
	log.Warn().Err(err).Msg("统一账户余额接口不可用，将在实际下单时验证权限")
	return nil
}

// GetFuturesBalance returns the available USDT balance from portfolio margin account.
func (b *BinanceFuturesClient) GetFuturesBalance(asset string) (decimal.Decimal, error) {
	balances, err := b.portfolioClient.NewGetBalanceService().Asset(asset).Do(context.Background())
	if err != nil {
		return decimal.Zero, err
	}

	for _, bal := range balances {
		if bal.Asset == asset {
			val, _ := decimal.NewFromString(bal.UMWalletBalance)
			return val, nil
		}
	}
	return decimal.Zero, fmt.Errorf("asset %s not found in portfolio account", asset)
}

// HasFuturesContract checks if a symbol exists in futures exchange info.
// Uses /fapi/ market data endpoint (not restricted by portfolio margin).
func (b *BinanceFuturesClient) HasFuturesContract(symbol string) bool {
	info, err := b.futuresClient.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		log.Error().Err(err).Msg("failed to get futures exchange info")
		return false
	}
	for _, s := range info.Symbols {
		if s.Symbol == symbol && s.Status == "TRADING" {
			return true
		}
	}
	return false
}

// OpenShort opens a short position via portfolio margin UM order (/papi/).
// 使用 PositionSide=SHORT 兼容双向持仓模式 (Hedge Mode)
func (b *BinanceFuturesClient) OpenShort(symbol string, quantity decimal.Decimal) (string, decimal.Decimal, error) {
	// Truncate to correct precision
	qty := b.truncateQty(symbol, quantity)
	log.Info().Str("symbol", symbol).Str("raw_qty", quantity.String()).Str("truncated_qty", qty.String()).Msg("OpenShort: qty precision adjusted")

	if !qty.IsPositive() {
		return "", decimal.Zero, fmt.Errorf("quantity %s truncated to zero, below minimum for %s", quantity.String(), symbol)
	}

	res, err := b.portfolioClient.NewUMOrderService().
		Symbol(symbol).
		Side(portfolio.SideTypeSell).
		PositionSide(portfolio.PositionSideTypeShort).
		Type(portfolio.OrderTypeMarket).
		Quantity(qty.String()).
		Do(context.Background())
	if err != nil {
		return "", decimal.Zero, err
	}

	avgPrice, _ := decimal.NewFromString(res.AvgPrice)
	return strconv.FormatInt(res.OrderID, 10), avgPrice, nil
}

// CloseShort closes a short position via portfolio margin UM order (/papi/).
// 使用 PositionSide=SHORT 兼容双向持仓模式 (Hedge Mode)
func (b *BinanceFuturesClient) CloseShort(symbol string, quantity decimal.Decimal) (string, decimal.Decimal, error) {
	qty := b.truncateQty(symbol, quantity)

	res, err := b.portfolioClient.NewUMOrderService().
		Symbol(symbol).
		Side(portfolio.SideTypeBuy).
		PositionSide(portfolio.PositionSideTypeShort).
		Type(portfolio.OrderTypeMarket).
		Quantity(qty.String()).
		Do(context.Background())
	if err != nil {
		return "", decimal.Zero, err
	}

	avgPrice, _ := decimal.NewFromString(res.AvgPrice)
	return strconv.FormatInt(res.OrderID, 10), avgPrice, nil
}

// truncateQty truncates quantity to the correct precision for the given futures symbol.
func (b *BinanceFuturesClient) truncateQty(symbol string, qty decimal.Decimal) decimal.Decimal {
	info, err := b.futuresClient.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get futures exchange info for precision, truncating to integer")
		return qty.Truncate(0)
	}

	for _, s := range info.Symbols {
		if s.Symbol == symbol {
			precision := int32(s.QuantityPrecision)
			truncated := qty.Truncate(precision)
			log.Debug().Str("symbol", symbol).Int32("precision", precision).Str("result", truncated.String()).Msg("Qty precision from exchange info")
			return truncated
		}
	}

	log.Warn().Str("symbol", symbol).Msg("Symbol not found in futures exchange info, truncating to integer")
	return qty.Truncate(0)
}

// SetLeverage sets the leverage for a symbol via portfolio margin (/papi/).
func (b *BinanceFuturesClient) SetLeverage(symbol string, leverage int) error {
	_, err := b.portfolioClient.NewChangeUMInitialLeverageService().
		Symbol(symbol).
		Leverage(leverage).
		Do(context.Background())
	return err
}

// FundingInfo holds funding rate and next settlement time for a symbol.
type FundingInfo struct {
	FundingRate     decimal.Decimal
	NextFundingTime int64 // unix milliseconds
}

// GetFundingInfo returns the current funding rate and next funding time.
// Uses /fapi/ market data endpoint (not restricted by portfolio margin).
func (b *BinanceFuturesClient) GetFundingInfo(symbol string) (*FundingInfo, error) {
	res, err := b.futuresClient.NewPremiumIndexService().Symbol(symbol).Do(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get premium index failed: %w", err)
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("no premium index data for %s", symbol)
	}
	rate, _ := decimal.NewFromString(res[0].LastFundingRate)
	return &FundingInfo{
		FundingRate:     rate,
		NextFundingTime: res[0].NextFundingTime,
	}, nil
}
