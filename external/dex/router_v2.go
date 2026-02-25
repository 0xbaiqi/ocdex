package dex

import (
	"context"
	"errors"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// PancakeRouterV2 Address
const PancakeRouterV2Address = "0x10ED43C718714eb63d5aA57B78B54704E256024E"

// GetAmountsOutV2 quotes using PancakeSwap V2 Router
func (d *DexClient) GetAmountsOutV2(tokenIn, tokenOut string, amountIn *big.Int) (*big.Int, error) {
	routerAddress := common.HexToAddress(PancakeRouterV2Address)

	const methodABI = `[{"constant":true,"inputs":[{"name":"amountIn","type":"uint256"},{"name":"path","type":"address[]"}],"name":"getAmountsOut","outputs":[{"name":"amounts","type":"uint256[]"}],"payable":false,"stateMutability":"view","type":"function"}]`

	parsedABI, err := abi.JSON(strings.NewReader(methodABI))
	if err != nil {
		return nil, err
	}

	path := []common.Address{
		common.HexToAddress(tokenIn),
		common.HexToAddress(tokenOut),
	}

	input, err := parsedABI.Pack("getAmountsOut", amountIn, path)
	if err != nil {
		return nil, err
	}

	msg := ethereum.CallMsg{
		To:   &routerAddress,
		Data: input,
	}

	output, err := d.client.CallContract(context.Background(), msg, nil)
	if err != nil {
		return nil, err
	}

	if len(output) == 0 {
		return nil, errors.New("V2 execution reverted (empty output)")
	}

	results, err := parsedABI.Unpack("getAmountsOut", output)
	if err != nil {
		return nil, err
	}

	// returns uint256[] amounts
	amounts := *abi.ConvertType(results[0], new([]*big.Int)).(*[]*big.Int)
	if len(amounts) < 2 {
		return nil, errors.New("insufficient output amounts")
	}

	// amounts[0] is input, amounts[1] is output
	return amounts[1], nil
}
