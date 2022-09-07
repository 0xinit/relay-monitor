package consensus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/protolambda/eth2api"
	"github.com/protolambda/eth2api/client/beaconapi"
	"github.com/protolambda/eth2api/client/validatorapi"
	"github.com/protolambda/zrnt/eth2/beacon/bellatrix"
	"github.com/protolambda/zrnt/eth2/beacon/common"
	"github.com/r3labs/sse/v2"
	"github.com/ralexstokes/relay-monitor/pkg/types"
	"go.uber.org/zap"
)

const clientTimeoutSec = 5

type ValidatorInfo struct {
	publicKey types.PublicKey
	index     types.ValidatorIndex
}

type Client struct {
	logger *zap.Logger
	client *eth2api.Eth2HttpClient

	proposerCache      map[types.Slot]ValidatorInfo
	proposerCacheMutex sync.RWMutex

	executionCache      map[types.Slot]types.Hash
	executionCacheMutex sync.RWMutex
}

func NewClient(ctx context.Context, endpoint string, logger *zap.Logger, currentSlot types.Slot, currentEpoch types.Epoch, slotsPerEpoch uint64) *Client {
	httpClient := &eth2api.Eth2HttpClient{
		Addr: endpoint,
		Cli: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 128,
			},
			Timeout: clientTimeoutSec * time.Second,
		},
		Codec: eth2api.JSONCodec{},
	}

	client := &Client{
		logger:         logger,
		client:         httpClient,
		proposerCache:  make(map[types.Slot]ValidatorInfo),
		executionCache: make(map[types.Slot]types.Hash),
	}

	err := client.loadCurrentContext(ctx, currentSlot, currentEpoch, slotsPerEpoch)
	if err != nil {
		logger := logger.Sugar()
		logger.Warn("could not load the current context from the consensus client")
	}

	return client
}

func (c *Client) loadCurrentContext(ctx context.Context, currentSlot types.Slot, currentEpoch types.Epoch, slotsPerEpoch uint64) error {
	logger := c.logger.Sugar()

	var baseSlot uint64
	if currentSlot > slotsPerEpoch {
		baseSlot = currentSlot - slotsPerEpoch
	}

	for i := baseSlot; i < slotsPerEpoch; i++ {
		_, err := c.FetchExecutionHash(ctx, i)
		if err != nil {
			logger.Warnf("could not fetch latest execution hash for slot %d: %v", currentSlot, err)
		}
	}

	err := c.FetchProposers(ctx, currentEpoch)
	if err != nil {
		logger.Warnf("could not load consensus state for epoch %d: %v", currentEpoch, err)
	}

	nextEpoch := currentEpoch + 1
	err = c.FetchProposers(ctx, nextEpoch)
	if err != nil {
		logger.Warnf("could not load consensus state for epoch %d: %v", nextEpoch, err)
	}

	return nil
}

func (c *Client) GetParentHash(ctx context.Context, slot types.Slot) (types.Hash, error) {
	targetSlot := slot - 1
	c.executionCacheMutex.RLock()
	parentHash, ok := c.executionCache[targetSlot]
	c.executionCacheMutex.RUnlock()
	if !ok {
		return c.FetchExecutionHash(ctx, targetSlot)
	}
	return parentHash, nil
}

func (c *Client) GetProposerPublicKey(ctx context.Context, slot types.Slot) (*types.PublicKey, error) {
	c.proposerCacheMutex.RLock()
	defer c.proposerCacheMutex.RUnlock()

	validator, ok := c.proposerCache[slot]
	if !ok {
		// TODO consider fallback to grab the assignments for the missing epoch...
		return nil, fmt.Errorf("missing proposer for slot %d", slot)
	}

	return &validator.publicKey, nil
}

func (c *Client) FetchProposers(ctx context.Context, epoch types.Epoch) error {
	var proposerDuties eth2api.DependentProposerDuty
	syncing, err := validatorapi.ProposerDuties(ctx, c.client, common.Epoch(epoch), &proposerDuties)
	if syncing {
		return fmt.Errorf("could not Fetch proposal duties in epoch %d because node is syncing", epoch)
	} else if err != nil {
		return err
	}

	// TODO handle reorgs, etc.
	c.proposerCacheMutex.Lock()
	for _, duty := range proposerDuties.Data {
		c.proposerCache[uint64(duty.Slot)] = ValidatorInfo{
			publicKey: types.PublicKey(duty.Pubkey),
			index:     uint64(duty.ValidatorIndex),
		}
	}
	c.proposerCacheMutex.Unlock()

	return nil
}

func (c *Client) backFillExecutionHash(ctx context.Context, slot types.Slot) (types.Hash, error) {
	for i := slot; i > 0; i-- {
		targetSlot := i - 1
		c.executionCacheMutex.RLock()
		executionHash, ok := c.executionCache[targetSlot]
		c.executionCacheMutex.RUnlock()
		if ok {
			for i := targetSlot; i < slot; i++ {
				c.executionCacheMutex.Lock()
				c.executionCache[i+1] = executionHash
				c.executionCacheMutex.Unlock()
			}
			return executionHash, nil
		}
	}
	return types.Hash{}, fmt.Errorf("no execution hashes present before %d (inclusive)", slot)
}

func (c *Client) FetchExecutionHash(ctx context.Context, slot types.Slot) (types.Hash, error) {
	// TODO handle reorgs, etc.
	c.executionCacheMutex.Lock()
	executionHash, ok := c.executionCache[slot]
	if ok {
		return executionHash, nil
	}
	c.executionCacheMutex.Unlock()

	blockID := eth2api.BlockIdSlot(slot)

	var signedBeaconBlock eth2api.VersionedSignedBeaconBlock
	exists, err := beaconapi.BlockV2(ctx, c.client, blockID, &signedBeaconBlock)
	if !exists {
		// TODO move search to `GetParentHash`
		// TODO also instantiate with first execution hash...
		return c.backFillExecutionHash(ctx, slot)
	} else if err != nil {
		return types.Hash{}, err
	}

	bellatrixBlock, ok := signedBeaconBlock.Data.(*bellatrix.SignedBeaconBlock)
	if !ok {
		return types.Hash{}, fmt.Errorf("could not parse block %s", signedBeaconBlock)
	}
	executionHash = types.Hash(bellatrixBlock.Message.Body.ExecutionPayload.BlockHash)

	// TODO handle reorgs, etc.
	c.executionCacheMutex.Lock()
	c.executionCache[slot] = executionHash
	c.executionCacheMutex.Unlock()

	return executionHash, nil
}

type headEvent struct {
	Slot  string     `json:"slot"`
	Block types.Root `json:"block"`
}

func (c *Client) StreamHeads(ctx context.Context) <-chan types.Coordinate {
	logger := c.logger.Sugar()

	sseClient := sse.NewClient(c.client.Addr + "/eth/v1/events?topics=head")
	ch := make(chan types.Coordinate, 1)
	go func() {
		err := sseClient.SubscribeRawWithContext(ctx, func(msg *sse.Event) {
			var event headEvent
			err := json.Unmarshal(msg.Data, &event)
			if err != nil {
				logger.Warnf("could not unmarshal `head` node event: %v", err)
				return
			}
			slot, err := strconv.Atoi(event.Slot)
			if err != nil {
				logger.Warnf("could not unmarshal slot from `head` node event: %v", err)
				return
			}
			head := types.Coordinate{
				Slot: types.Slot(slot),
				Root: event.Block,
			}
			ch <- head
		})
		if err != nil {
			logger.Errorw("could not subscribe to head event", "error", err)
		}
	}()
	return ch
}
