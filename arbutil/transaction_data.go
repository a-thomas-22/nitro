// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package arbutil

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

func GetLogTransaction(ctx context.Context, client *ethclient.Client, log types.Log) (*types.Transaction, error) {
	tx, err := client.TransactionInBlock(ctx, log.BlockHash, log.TxIndex)
	if err != nil {
		return nil, err
	}
	if tx.Hash() != log.TxHash {
		return nil, fmt.Errorf("L1 client returned unexpected transaction hash %v when looking up block %v transaction %v with expected hash %v", tx.Hash(), log.BlockHash, log.TxIndex, log.TxHash)
	}
	return tx, nil
}

// GetLogEmitterTxData requires that the tx's data is at least 4 bytes long
func GetLogEmitterTxData(ctx context.Context, client *ethclient.Client, log types.Log) ([]byte, error) {
	tx, err := GetLogTransaction(ctx, client, log)
	if err != nil {
		return nil, err
	}
	if len(tx.Data()) < 4 {
		return nil, fmt.Errorf("log emitting transaction %v unexpectedly does not have enough data", tx.Hash())
	}
	return tx.Data(), nil
}
