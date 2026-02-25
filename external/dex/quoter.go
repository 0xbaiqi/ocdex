package dex

import (
	"ocdex/external/dex/pancake"
	"context"
	"errors"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// PancakeQuoterV2 Address on BSC
const PancakeQuoterV2Address = "0xB048Bbc1Ee6b733FF8cf8B8E3138C6962d293581"

// GetAmountOut quotes the output amount for a given input amount using PancakeSwap V3 Quoter
// We use manual ABI packing + CallContract to force a static call (view) instead of a transaction.
func (d *DexClient) GetAmountOut(tokenIn, tokenOut string, amountIn *big.Int, fee *big.Int) (*big.Int, error) {
	quoterAddress := common.HexToAddress(PancakeQuoterV2Address)

	// 1. Pack the input arguments
	// quoteExactInputSingle((address,address,uint256,uint24,uint160))
	parsedABI, err := abi.JSON(strings.NewReader(pancake.QuoterV2MetaData.ABI))
	if err != nil {
		return nil, err
	}

	params := pancake.IQuoterV2QuoteExactInputSingleParams{
		TokenIn:           common.HexToAddress(tokenIn),
		TokenOut:          common.HexToAddress(tokenOut),
		AmountIn:          amountIn,
		Fee:               fee,
		SqrtPriceLimitX96: big.NewInt(0),
	}

	input, err := parsedABI.Pack("quoteExactInputSingle", params)
	if err != nil {
		return nil, err
	}

	// 2. Perform Static Call (eth_call)
	msg := ethereum.CallMsg{
		To:   &quoterAddress,
		Data: input,
	}

	output, err := d.client.CallContract(context.Background(), msg, nil)
	if err != nil {
		return nil, err
	}

	if len(output) == 0 {
		return nil, errors.New("DEX execution reverted (empty output). Check Pair existence or fee tier")
	}

	// 3. Unpack the output
	// returns (uint256 amountOut, uint160 sqrtPriceX96After, uint32 initializedTicksCrossed, uint256 gasEstimate)
	results, err := parsedABI.Unpack("quoteExactInputSingle", output)
	if err != nil {
		return nil, err
	}

	// Result[0] is amountOut
	return results[0].(*big.Int), nil
}
