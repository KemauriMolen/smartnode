package rewards

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/goccy/go-json"
	bserv "github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/ipfs/go-merkledag"
	"github.com/klauspost/compress/zstd"
	"github.com/mitchellh/go-homedir"
	batchquery "github.com/rocket-pool/batch-query"
	"github.com/rocket-pool/rocketpool-go/rewards"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	rpstate "github.com/rocket-pool/rocketpool-go/utils/state"
	"github.com/rocket-pool/smartnode/rocketpool/common/beacon"
	"github.com/rocket-pool/smartnode/shared/config"
	cfgtypes "github.com/rocket-pool/smartnode/shared/types/config"
	"github.com/web3-storage/go-w3s-client/adder"
)

// Settings
const (
	FarEpoch                uint64 = 18446744073709551615
	LegacyDetailsBatchCount int    = 200
)

// Simple container for the zero value so it doesn't have to be recreated over and over
var zero *big.Int

type ClaimStatus struct {
	Claimed   []uint64
	Unclaimed []uint64
}

// Gets the intervals the node can claim and the intervals that have already been claimed
func GetClaimStatus(rp *rocketpool.RocketPool, nodeAddress common.Address, currentIndex uint64) (*ClaimStatus, error) {
	// Get the claim status of every interval that's happened so far
	one := big.NewInt(1)
	bucket := currentIndex / 256

	// Get the bucket bitmaps
	bucketBitmaps := make([]*big.Int, bucket+1)
	err := rp.Query(func(mc *batchquery.MultiCaller) error {
		for i := uint64(0); i <= bucket; i++ {
			bucketBig := big.NewInt(int64(i))
			bucketBytes := [32]byte{}
			bucketBig.FillBytes(bucketBytes[:])
			key := crypto.Keccak256Hash([]byte("rewards.interval.claimed"), nodeAddress.Bytes(), bucketBytes[:])
			rp.Storage.GetUint(mc, &bucketBitmaps[i], key)
		}
		return nil
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("error getting reward bucket bitmaps: %w", err)
	}

	// Get the reward status per bucket
	claimStatus := &ClaimStatus{}
	for i := uint64(0); i <= bucket; i++ {
		bitmap := bucketBitmaps[i]
		for j := uint64(0); j < 256; j++ {
			targetIndex := i*256 + j
			if targetIndex >= currentIndex {
				// End once we've hit the current interval
				break
			}

			mask := big.NewInt(0)
			mask.Lsh(one, uint(j))
			maskedBitmap := big.NewInt(0)
			maskedBitmap.And(bitmap, mask)

			if maskedBitmap.Cmp(mask) == 0 {
				// This bit was flipped, so it's been claimed already
				claimStatus.Claimed = append(claimStatus.Claimed, targetIndex)
			} else {
				// This bit was not flipped, so it hasn't been claimed yet
				claimStatus.Unclaimed = append(claimStatus.Unclaimed, targetIndex)
			}
		}
	}

	return claimStatus, nil
}

// Gets the information for an interval including the file status, the validity, and the node's rewards
func GetIntervalInfo(rp *rocketpool.RocketPool, cfg *config.RocketPoolConfig, nodeAddress common.Address, interval uint64, opts *bind.CallOpts) (info IntervalInfo, err error) {
	info.Index = interval
	var event rewards.RewardsEvent

	if cfg.Smartnode.Network.Value.(cfgtypes.Network) == cfgtypes.Network_Prater && interval < 6 {
		// Use the hardcoded prehistoric lookup for early Prater intervals
		event = praterPrehistoryIntervalEvents[interval]
	} else {
		// Get the event details for this interval
		event, err = GetRewardSnapshotEvent(rp, cfg, interval, opts)
		if err != nil {
			return
		}
	}

	info.CID = event.MerkleTreeCID
	info.StartTime = event.IntervalStartTime
	info.EndTime = event.IntervalEndTime
	merkleRootCanon := event.MerkleRoot

	// Check if the tree file exists
	info.TreeFilePath = cfg.Smartnode.GetRewardsTreePath(interval, true)
	_, err = os.Stat(info.TreeFilePath)
	if os.IsNotExist(err) {
		info.TreeFileExists = false
		err = nil
		return
	}
	info.TreeFileExists = true

	// Unmarshal it
	fileBytes, err := os.ReadFile(info.TreeFilePath)
	if err != nil {
		err = fmt.Errorf("error reading %s: %w", info.TreeFilePath, err)
		return
	}
	var proofWrapper RewardsFile
	err = json.Unmarshal(fileBytes, &proofWrapper)
	if err != nil {
		err = fmt.Errorf("error deserializing %s: %w", info.TreeFilePath, err)
		return
	}

	// Make sure the Merkle root has the expected value
	merkleRootFromFile := common.HexToHash(proofWrapper.MerkleRoot)
	if merkleRootCanon != merkleRootFromFile {
		info.MerkleRootValid = false
		return
	}
	info.MerkleRootValid = true

	// Get the rewards from it
	rewards, exists := proofWrapper.NodeRewards[nodeAddress]
	info.NodeExists = exists
	if exists {
		info.CollateralRplAmount = rewards.CollateralRpl
		info.ODaoRplAmount = rewards.OracleDaoRpl
		info.SmoothingPoolEthAmount = rewards.SmoothingPoolEth

		var proof []common.Hash
		proof, err = rewards.GetMerkleProof()
		if err != nil {
			err = fmt.Errorf("error deserializing merkle proof for %s, node %s: %w", info.TreeFilePath, nodeAddress.Hex(), err)
			return
		}
		info.MerkleProof = proof
	}

	return
}

// Get the event for a rewards snapshot
func GetRewardSnapshotEvent(rp *rocketpool.RocketPool, cfg *config.RocketPoolConfig, interval uint64, opts *bind.CallOpts) (rewards.RewardsEvent, error) {
	addresses := cfg.Smartnode.GetPreviousRewardsPoolAddresses()
	rewardsPool, err := rewards.NewRewardsPool(rp)
	if err != nil {
		return rewards.RewardsEvent{}, fmt.Errorf("error getting rewards pool binding: %w", err)
	}

	found, event, err := rewardsPool.GetRewardsEvent(rp, interval, addresses, opts)
	if err != nil {
		return rewards.RewardsEvent{}, fmt.Errorf("error getting rewards event for interval %d: %w", interval, err)
	}
	if !found {
		return rewards.RewardsEvent{}, fmt.Errorf("interval %d event not found", interval)
	}

	return event, nil
}

// Get the number of the latest EL block that was created before the given timestamp
func GetELBlockHeaderForTime(targetTime time.Time, rp *rocketpool.RocketPool) (*types.Header, error) {
	// Get the latest block's timestamp
	latestBlockHeader, err := rp.Client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("error getting latest block header: %w", err)
	}
	latestBlock := latestBlockHeader.Number

	// Get half the distance between the protocol deployment and right now
	delta := big.NewInt(0).Sub(latestBlock, rp.DeployBlock)
	delta.Div(delta, big.NewInt(2))

	// Start at the halfway point
	candidateBlockNumber := big.NewInt(0).Sub(latestBlock, delta)
	candidateBlock, err := rp.Client.HeaderByNumber(context.Background(), candidateBlockNumber)
	if err != nil {
		return nil, fmt.Errorf("error getting EL block %d: %w", candidateBlock, err)
	}
	bestBlock := candidateBlock
	pivotSize := candidateBlock.Number.Uint64()
	minimumDistance := +math.Inf(1)
	targetTimeUnix := float64(targetTime.Unix())

	for {
		// Get the distance from the candidate block to the target time
		candidateTime := float64(candidateBlock.Time)
		delta := targetTimeUnix - candidateTime
		distance := math.Abs(delta)

		// If it's better, replace the best candidate with it
		if distance < minimumDistance {
			minimumDistance = distance
			bestBlock = candidateBlock
		} else if pivotSize == 1 {
			// If the pivot is down to size 1 and we didn't find anything better after another iteration, this is the best block!
			for candidateTime > targetTimeUnix {
				// Get the previous block if this one happened after the target time
				candidateBlockNumber.Sub(candidateBlockNumber, big.NewInt(1))
				candidateBlock, err = rp.Client.HeaderByNumber(context.Background(), candidateBlockNumber)
				if err != nil {
					return nil, fmt.Errorf("error getting EL block %d: %w", candidateBlock, err)
				}
				candidateTime = float64(candidateBlock.Time)
				bestBlock = candidateBlock
			}
			return bestBlock, nil
		}

		// Iterate over the correct half, setting the pivot to the halfway point of that half (rounded up)
		pivotSize = uint64(math.Ceil(float64(pivotSize) / 2))
		if delta < 0 {
			// Go left
			candidateBlockNumber.Sub(candidateBlockNumber, big.NewInt(int64(pivotSize)))
		} else {
			// Go right
			candidateBlockNumber.Add(candidateBlockNumber, big.NewInt(int64(pivotSize)))
		}

		// Clamp the new candidate to the latest block
		if candidateBlockNumber.Uint64() > (latestBlock.Uint64() - 1) {
			candidateBlockNumber.SetUint64(latestBlock.Uint64() - 1)
		}

		candidateBlock, err = rp.Client.HeaderByNumber(context.Background(), candidateBlockNumber)
		if err != nil {
			return nil, fmt.Errorf("error getting EL block %d: %w", candidateBlock, err)
		}
	}
}

// Downloads a single rewards file
func DownloadRewardsFile(cfg *config.RocketPoolConfig, interval uint64, cid string, isDaemon bool) error {

	// Determine file name and path
	rewardsTreePath, err := homedir.Expand(cfg.Smartnode.GetRewardsTreePath(interval, isDaemon))
	if err != nil {
		return fmt.Errorf("error expanding rewards tree path: %w", err)
	}
	rewardsTreeFilename := filepath.Base(rewardsTreePath)
	ipfsFilename := rewardsTreeFilename + config.RewardsTreeIpfsExtension

	// Create URL list
	urls := []string{
		fmt.Sprintf(config.PrimaryRewardsFileUrl, cid, ipfsFilename),
		fmt.Sprintf(config.SecondaryRewardsFileUrl, cid, ipfsFilename),
		fmt.Sprintf(config.GithubRewardsFileUrl, string(cfg.Smartnode.Network.Value.(cfgtypes.Network)), rewardsTreeFilename),
	}

	// Attempt downloads
	errBuilder := strings.Builder{}
	for _, url := range urls {
		resp, err := http.Get(url)
		if err != nil {
			errBuilder.WriteString(fmt.Sprintf("Downloading %s failed (%s)\n", url, err.Error()))
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errBuilder.WriteString(fmt.Sprintf("Downloading %s failed with status %s\n", url, resp.Status))
			continue
		} else {
			// If we got here, we have a successful download
			bytes, err := io.ReadAll(resp.Body)
			if err != nil {
				errBuilder.WriteString(fmt.Sprintf("Error reading response bytes from %s: %s\n", url, err.Error()))
				continue
			}

			writeBytes := bytes
			if strings.HasSuffix(url, config.RewardsTreeIpfsExtension) {
				// Decompress it
				writeBytes, err = decompressFile(bytes)
				if err != nil {
					errBuilder.WriteString(fmt.Sprintf("Error decompressing %s: %s\n", url, err.Error()))
					continue
				}
			}

			// Write the file
			err = os.WriteFile(rewardsTreePath, writeBytes, 0644)
			if err != nil {
				return fmt.Errorf("error saving interval %d file to %s: %w", interval, rewardsTreePath, err)
			}
			return nil
		}
	}

	return fmt.Errorf(errBuilder.String())

}

// Get the IPFS CID for a blob of data
func GetCidForRewardsFile(rewardsFile *RewardsFile, filename string) (cid.Cid, error) {
	// Encode the rewards file in JSON
	data, err := json.Marshal(rewardsFile)
	if err != nil {
		return cid.Cid{}, fmt.Errorf("error serializing rewards file: %w", err)
	}

	// Compress the data
	encoder, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	compressedData := encoder.EncodeAll(data, make([]byte, 0, len(data)))

	// Create an in-memory file and FS
	mapFile := fstest.MapFile{
		Data:    compressedData,
		Mode:    0644,
		ModTime: time.Now(),
	}
	fsMap := fstest.MapFS{filename: &mapFile}
	file, err := fsMap.Open(filename)
	if err != nil {
		return cid.Cid{}, fmt.Errorf("error opening memory-mapped file: %w", err)
	}

	// Use the web3.storage libraries to chunk the data and get the root CID
	ds := dssync.MutexWrap(datastore.NewMapDatastore())
	bsvc := bserv.New(blockstore.NewBlockstore(ds), nil)
	dag := merkledag.NewDAGService(bsvc)
	dagFmtr, err := adder.NewAdder(context.Background(), dag)
	if err != nil {
		return cid.Cid{}, fmt.Errorf("error creating DAG adder: %w", err)
	}
	root, err := dagFmtr.Add(file, "", fsMap)
	if err != nil {
		return cid.Cid{}, fmt.Errorf("error adding rewards file to DAG: %w", err)
	}

	return root, err
}

// Decompresses a rewards file
func decompressFile(compressedBytes []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("error creating compression decoder: %w", err)
	}

	decompressedBytes, err := decoder.DecodeAll(compressedBytes, nil)
	if err != nil {
		return nil, fmt.Errorf("error decompressing rewards file: %w", err)
	}

	return decompressedBytes, nil
}

// Gets the start slot for the given interval
func GetStartSlotForInterval(previousIntervalEvent rewards.RewardsEvent, bc beacon.Client, beaconConfig beacon.Eth2Config) (uint64, error) {
	// Get the chain head
	head, err := bc.GetBeaconHead()
	if err != nil {
		return 0, fmt.Errorf("error getting Beacon chain head: %w", err)
	}

	// Sanity check to confirm the BN can access the block from the previous interval
	_, exists, err := bc.GetBeaconBlock(previousIntervalEvent.ConsensusBlock.String())
	if err != nil {
		return 0, fmt.Errorf("error verifying block from previous interval: %w", err)
	}
	if !exists {
		return 0, fmt.Errorf("couldn't retrieve CL block from previous interval (slot %d); this likely means you checkpoint sync'd your Beacon Node and it has not backfilled to the previous interval yet so it cannot be used for tree generation", previousIntervalEvent.ConsensusBlock.Uint64())
	}

	previousEpoch := previousIntervalEvent.ConsensusBlock.Uint64() / beaconConfig.SlotsPerEpoch
	nextEpoch := previousEpoch + 1
	consensusStartBlock := nextEpoch * beaconConfig.SlotsPerEpoch

	// Get the first block that isn't missing
	currentEpoch := consensusStartBlock / beaconConfig.SlotsPerEpoch
	found := false
	for currentEpoch <= head.Epoch {
		_, exists, err := bc.GetBeaconBlock(fmt.Sprint(consensusStartBlock))
		if err != nil {
			return 0, fmt.Errorf("error getting EL data for BC slot %d: %w", consensusStartBlock, err)
		}
		if !exists {
			consensusStartBlock++
			currentEpoch = consensusStartBlock / beaconConfig.SlotsPerEpoch
		} else {
			found = true
			break
		}
	}

	// If we've processed all of the blocks up to the chain head and still didn't find it, error out
	if !found {
		return 0, fmt.Errorf("scanned up to the chain head (Beacon block %d) but none of the blocks were found.", consensusStartBlock)
	}

	return consensusStartBlock, nil
}

// Get the bond and node fee of a minipool for the specified time
func getMinipoolBondAndNodeFee(details *rpstate.NativeMinipoolDetails, blockTime time.Time) (*big.Int, *big.Int) {
	currentBond := details.NodeDepositBalance
	currentFee := details.NodeFee
	previousBond := details.LastBondReductionPrevValue
	previousFee := details.LastBondReductionPrevNodeFee

	// Init the zero wrapper
	if zero == nil {
		zero = big.NewInt(0)
	}

	var reductionTimeBig *big.Int = details.LastBondReductionTime
	if reductionTimeBig.Cmp(zero) == 0 {
		// Never reduced
		return currentBond, currentFee
	} else {
		reductionTime := time.Unix(reductionTimeBig.Int64(), 0)
		if reductionTime.Sub(blockTime) > 0 {
			// This block occurred before the reduction
			if previousFee.Cmp(zero) == 0 {
				// Catch for minipools that were created before this call existed
				return previousBond, currentFee
			}
			return previousBond, previousFee
		}
	}

	return currentBond, currentFee
}