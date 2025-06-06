package pruning

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state/pruner"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"

	protocol "github.com/offchainlabs/bold/chain-abstraction"
	boldrollup "github.com/offchainlabs/bold/solgen/go/rollupgen"
	"github.com/offchainlabs/nitro/arbnode"
	"github.com/offchainlabs/nitro/arbnode/dataposter/storage"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/cmd/chaininfo"
	"github.com/offchainlabs/nitro/cmd/conf"
	"github.com/offchainlabs/nitro/execution/gethexec"
	"github.com/offchainlabs/nitro/solgen/go/bridgegen"
	"github.com/offchainlabs/nitro/staker"
	boldstaker "github.com/offchainlabs/nitro/staker/bold"
	legacystaker "github.com/offchainlabs/nitro/staker/legacy"
	multiprotocolstaker "github.com/offchainlabs/nitro/staker/multi_protocol"
)

type importantRoots struct {
	chainDb ethdb.Database
	roots   []common.Hash
	heights []uint64
}

// The minimum block distance between two important roots
const minRootDistance = 2000

// Marks a header as important, and records its root and height.
// If overwrite is true, it'll remove any future roots and replace them with this header.
// If overwrite is false, it'll ignore this header if it has future roots.
func (r *importantRoots) addHeader(header *types.Header, overwrite bool) error {
	targetBlockNum := header.Number.Uint64()
	for {
		if header == nil || header.Root == (common.Hash{}) {
			log.Error("missing state of pruning target", "blockNum", targetBlockNum)
			return nil
		}
		exists, err := r.chainDb.Has(header.Root.Bytes())
		if err != nil {
			return err
		}
		if exists {
			break
		}
		num := header.Number.Uint64()
		if num%3000 == 0 {
			log.Info("looking for old block with state to keep", "current", num, "target", targetBlockNum)
		}
		// An underflow is fine here because it'll just return nil due to not found
		header = rawdb.ReadHeader(r.chainDb, header.ParentHash, num-1)
	}
	height := header.Number.Uint64()
	for len(r.heights) > 0 && r.heights[len(r.heights)-1] > height {
		if !overwrite {
			return nil
		}
		r.roots = r.roots[:len(r.roots)-1]
		r.heights = r.heights[:len(r.heights)-1]
	}
	if len(r.heights) > 0 && r.heights[len(r.heights)-1]+minRootDistance > height {
		return nil
	}
	r.roots = append(r.roots, header.Root)
	r.heights = append(r.heights, height)
	return nil
}

var hashListRegex = regexp.MustCompile("^(0x)?[0-9a-fA-F]{64}(,(0x)?[0-9a-fA-F]{64})*$")

// Finds important roots to retain while proving
func findImportantRoots(ctx context.Context, chainDb ethdb.Database, stack *node.Node, initConfig *conf.InitConfig, cacheConfig *core.CacheConfig, persistentConfig *conf.PersistentConfig, l1Client *ethclient.Client, rollupAddrs chaininfo.RollupAddresses, validatorRequired bool) ([]common.Hash, error) {
	chainConfig := gethexec.TryReadStoredChainConfig(chainDb)
	if chainConfig == nil {
		return nil, errors.New("database doesn't have a chain config (was this node initialized?)")
	}
	arbDb, err := stack.OpenDatabaseWithExtraOptions("arbitrumdata", 0, 0, "arbitrumdata/", true, persistentConfig.Pebble.ExtraOptions("arbitrumdata"))
	if err != nil {
		return nil, err
	}
	defer func() {
		err := arbDb.Close()
		if err != nil {
			log.Warn("failed to close arbitrum database after finding pruning targets", "err", err)
		}
	}()
	roots := importantRoots{
		chainDb: chainDb,
	}
	genesisNum := chainConfig.ArbitrumChainParams.GenesisBlockNum
	genesisHash := rawdb.ReadCanonicalHash(chainDb, genesisNum)
	genesisHeader := rawdb.ReadHeader(chainDb, genesisHash, genesisNum)
	if genesisHeader == nil {
		return nil, errors.New("missing L2 genesis block header")
	}
	err = roots.addHeader(genesisHeader, false)
	if err != nil {
		return nil, err
	}
	if initConfig.Prune == "validator" {
		if l1Client == nil || reflect.ValueOf(l1Client).IsNil() {
			return nil, errors.New("an L1 connection is required for validator pruning")
		}
		confirmedHash, err := getLatestConfirmedHash(ctx, rollupAddrs, l1Client)
		if err != nil {
			return nil, err
		}
		confirmedNumber := rawdb.ReadHeaderNumber(chainDb, confirmedHash)
		var confirmedHeader *types.Header
		if confirmedNumber != nil {
			confirmedHeader = rawdb.ReadHeader(chainDb, confirmedHash, *confirmedNumber)
		}
		if confirmedHeader != nil {
			err = roots.addHeader(confirmedHeader, false)
			if err != nil {
				return nil, err
			}
		} else {
			log.Warn("missing latest confirmed block", "hash", confirmedHash)
		}

		validatorDb := rawdb.NewTable(arbDb, storage.BlockValidatorPrefix)
		lastValidated, err := staker.ReadLastValidatedInfo(validatorDb)
		if err != nil {
			return nil, err
		}
		if lastValidated != nil {
			var lastValidatedHeader *types.Header
			headerNum := rawdb.ReadHeaderNumber(chainDb, lastValidated.GlobalState.BlockHash)
			if headerNum != nil {
				lastValidatedHeader = rawdb.ReadHeader(chainDb, lastValidated.GlobalState.BlockHash, *headerNum)
			}
			if lastValidatedHeader != nil {
				err = roots.addHeader(lastValidatedHeader, false)
				if err != nil {
					return nil, err
				}
			} else {
				log.Warn("missing latest validated block", "hash", lastValidated.GlobalState.BlockHash)
			}
		}
	} else if initConfig.Prune == "full" || initConfig.Prune == "minimal" {
		if validatorRequired {
			return nil, fmt.Errorf("refusing to prune in %s mode when validator is enabled (you should use \"validator\" pruning mode)", initConfig.Prune)
		}
	} else if hashListRegex.MatchString(initConfig.Prune) {
		parts := strings.Split(initConfig.Prune, ",")
		roots := []common.Hash{genesisHeader.Root}
		for _, part := range parts {
			root := common.HexToHash(part)
			if root == genesisHeader.Root {
				// This was already included in the builtin list
				continue
			}
			roots = append(roots, root)
		}
		return roots, nil
	} else {
		return nil, fmt.Errorf("unknown pruning mode: \"%v\"", initConfig.Prune)
	}
	if initConfig.Prune != "minimal" && l1Client != nil {
		// in pruning modes other then "minimal", find the latest finalized block and add it as a pruning target
		l1Block, err := l1Client.BlockByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
		if err != nil {
			return nil, fmt.Errorf("failed to get finalized block: %w", err)
		}
		l1BlockNum := l1Block.NumberU64()
		tracker, err := arbnode.NewInboxTracker(arbDb, nil, nil, arbnode.DefaultSnapSyncConfig)
		if err != nil {
			return nil, err
		}
		batch, err := tracker.GetBatchCount()
		if err != nil {
			return nil, err
		}
		for {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if batch == 0 {
				// No batch has been finalized
				break
			}
			batch -= 1
			meta, err := tracker.GetBatchMetadata(batch)
			if err != nil {
				return nil, err
			}
			if meta.ParentChainBlock <= l1BlockNum {
				signedBlockNum := arbutil.MessageCountToBlockNumber(meta.MessageCount, genesisNum)
				// #nosec G115
				blockNum := uint64(signedBlockNum)
				l2Hash := rawdb.ReadCanonicalHash(chainDb, blockNum)
				l2Header := rawdb.ReadHeader(chainDb, l2Hash, blockNum)
				if l2Header == nil {
					log.Warn("latest finalized L2 block is unknown", "blockNum", signedBlockNum)
					break
				}
				err = roots.addHeader(l2Header, false)
				if err != nil {
					return nil, err
				}
				break
			}
		}
	}
	roots.roots = append(roots.roots, common.Hash{}) // the latest snapshot
	log.Info("found pruning target blocks", "heights", roots.heights, "roots", roots.roots)
	return roots.roots, nil
}

func getLatestConfirmedHash(ctx context.Context, rollupAddrs chaininfo.RollupAddresses, l1Client *ethclient.Client) (common.Hash, error) {
	callOpts := bind.CallOpts{
		Context:     ctx,
		BlockNumber: big.NewInt(int64(rpc.FinalizedBlockNumber)),
	}
	bridge, err := bridgegen.NewIBridge(rollupAddrs.Bridge, l1Client)
	if err != nil {
		return common.Hash{}, err
	}
	isBoldActive, rollupAddress, err := multiprotocolstaker.IsBoldActive(&callOpts, bridge, l1Client)
	if err != nil {
		return common.Hash{}, err
	}
	if isBoldActive {
		rollupUserLogic, err := boldrollup.NewRollupUserLogic(rollupAddress, l1Client)
		if err != nil {
			return common.Hash{}, err
		}
		latestConfirmed, err := rollupUserLogic.LatestConfirmed(&callOpts)
		if err != nil {
			return common.Hash{}, err
		}
		assertion, err := boldstaker.ReadBoldAssertionCreationInfo(
			ctx,
			rollupUserLogic,
			l1Client,
			rollupAddress,
			latestConfirmed,
		)
		if err != nil {
			return common.Hash{}, err
		}
		return protocol.GoGlobalStateFromSolidity(assertion.AfterState.GlobalState).BlockHash, nil
	} else {
		rollup, err := legacystaker.NewRollupWatcher(rollupAddrs.Rollup, l1Client, callOpts)
		if err != nil {
			return common.Hash{}, err
		}
		latestConfirmedNum, err := rollup.LatestConfirmed(&callOpts)
		if err != nil {
			return common.Hash{}, err
		}
		latestConfirmedNode, err := rollup.LookupNode(ctx, latestConfirmedNum)
		if err != nil {
			return common.Hash{}, err
		}
		return latestConfirmedNode.Assertion.AfterState.GlobalState.BlockHash, nil
	}
}

func PruneChainDb(ctx context.Context, chainDb ethdb.Database, stack *node.Node, initConfig *conf.InitConfig, cacheConfig *core.CacheConfig, persistentConfig *conf.PersistentConfig, l1Client *ethclient.Client, rollupAddrs chaininfo.RollupAddresses, validatorRequired bool) error {
	if cacheConfig.StateScheme == rawdb.PathScheme {
		return nil
	}

	if initConfig.Prune == "" {
		return pruner.RecoverPruning(stack.InstanceDir(), chainDb, initConfig.PruneThreads)
	}
	root, err := findImportantRoots(ctx, chainDb, stack, initConfig, cacheConfig, persistentConfig, l1Client, rollupAddrs, validatorRequired)
	if err != nil {
		return fmt.Errorf("failed to find root to retain for pruning: %w", err)
	}

	pruner, err := pruner.NewPruner(chainDb, pruner.Config{Datadir: stack.InstanceDir(), BloomSize: initConfig.PruneBloomSize, Threads: initConfig.PruneThreads, CleanCacheSize: initConfig.PruneTrieCleanCache, ParallelStorageTraversal: initConfig.PruneParallelStorageTraversal})
	if err != nil {
		return err
	}
	return pruner.Prune(root)
}
