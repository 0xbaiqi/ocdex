package dex

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
)

type DexClient struct {
	client *ethclient.Client
	url    string
}

func NewDexClient(rpcURL string) (*DexClient, error) {
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		log.Error().Err(err).Msg("Failed to connect to DEX RPC")
		return nil, err
	}
	return &DexClient{
		client: client,
		url:    rpcURL,
	}, nil
}

func (d *DexClient) GetBlockNumber(ctx context.Context) (uint64, error) {
	return d.client.BlockNumber(ctx)
}

func (d *DexClient) GetGasPrice(ctx context.Context) (*big.Int, error) {
	return d.client.SuggestGasPrice(ctx)
}
