package cex

import (
	"ocdex/config"
	"context"
	"fmt"

	"github.com/adshao/go-binance/v2"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

type BinanceClient struct {
	client *binance.Client
}

func NewBinanceClient(cfg config.APIConfig) *BinanceClient {
	client := binance.NewClient(cfg.ApiKey, cfg.SecretKey)
	return &BinanceClient{client: client}
}

func (b *BinanceClient) GetPrice(symbol string) (string, error) {
	prices, err := b.client.NewListPricesService().Symbol(symbol).Do(context.Background())
	if err != nil {
		log.Error().Err(err).Str("symbol", symbol).Msg("Failed to fetch price from Binance")
		return "", err
	}

	if len(prices) > 0 {
		return prices[0].Price, nil
	}
	return "", fmt.Errorf("no price data for %s", symbol)
}

// CheckConnectivity verifies if we can reach Binance API
func (b *BinanceClient) CheckConnectivity() error {
	err := b.client.NewPingService().Do(context.Background())
	return err
}

// GetBalance returns the free balance of a specific asset
func (b *BinanceClient) GetBalance(asset string) (decimal.Decimal, error) {
	res, err := b.client.NewGetAccountService().Do(context.Background())
	if err != nil {
		return decimal.Zero, err
	}

	for _, bal := range res.Balances {
		if bal.Asset == asset {
			return decimal.NewFromString(bal.Free)
		}
	}
	return decimal.Zero, nil
}

// PlaceMarketBuyOrder creates a MARKET BUY order using quoteOrderQty (spend exact USDT amount).
func (b *BinanceClient) PlaceMarketBuyOrder(symbol, quoteQty string) (int64, error) {
	order, err := b.client.NewCreateOrderService().
		Symbol(symbol).
		Side(binance.SideTypeBuy).
		Type(binance.OrderTypeMarket).
		QuoteOrderQty(quoteQty).
		Do(context.Background())

	if err != nil {
		return 0, err
	}
	return order.OrderID, nil
}

// PlaceMarketSellOrder creates a MARKET SELL order with auto LOT_SIZE precision.
func (b *BinanceClient) PlaceMarketSellOrder(symbol, quantity string) (int64, error) {
	// Truncate quantity to LOT_SIZE step size
	qty := b.truncateSpotQty(symbol, quantity)

	// Skip if truncated to zero (below minimum lot size)
	qtyDec, _ := decimal.NewFromString(qty)
	if !qtyDec.IsPositive() {
		log.Warn().Str("symbol", symbol).Str("original", quantity).Msg("Spot sell skipped: qty below minimum lot size after truncation")
		return 0, nil
	}

	order, err := b.client.NewCreateOrderService().
		Symbol(symbol).
		Side(binance.SideTypeSell).
		Type(binance.OrderTypeMarket).
		Quantity(qty).
		Do(context.Background())

	if err != nil {
		return 0, err
	}
	return order.OrderID, nil
}

// truncateSpotQty truncates quantity string to match LOT_SIZE stepSize from exchange info.
func (b *BinanceClient) truncateSpotQty(symbol, quantity string) string {
	qty, err := decimal.NewFromString(quantity)
	if err != nil {
		return quantity
	}

	info, err := b.client.NewExchangeInfoService().Symbol(symbol).Do(context.Background())
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get spot exchange info, truncating to integer")
		return qty.Truncate(0).String()
	}

	for _, s := range info.Symbols {
		if s.Symbol == symbol {
			for _, f := range s.Filters {
				if f["filterType"] == "LOT_SIZE" {
					stepStr, ok := f["stepSize"].(string)
					if !ok {
						break
					}
					step, _ := decimal.NewFromString(stepStr)
					if step.IsZero() {
						break
					}
					// Calculate precision from stepSize (e.g. "1" → 0, "0.1" → 1, "0.01" → 2)
					precision := int32(0)
					for step.LessThan(decimal.NewFromInt(1)) {
						precision++
						step = step.Mul(decimal.NewFromInt(10))
					}
					truncated := qty.Truncate(precision)
					log.Debug().Str("symbol", symbol).Int32("precision", precision).Str("stepSize", stepStr).Str("result", truncated.String()).Msg("Spot qty precision from LOT_SIZE")
					return truncated.String()
				}
			}
		}
	}

	log.Warn().Str("symbol", symbol).Msg("LOT_SIZE filter not found, truncating to integer")
	return qty.Truncate(0).String()
}
