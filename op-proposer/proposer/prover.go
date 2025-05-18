package proposer

import (
	"context"
	"fmt"

	"github.com/base/op-enclave/op-enclave/enclave"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/predeploys"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/hashicorp/go-multierror"
)

type Prover struct {
	config      *enclave.PerChainConfig
	chainConfig *params.ChainConfig
	configHash  common.Hash
	l1          L1Client
	l2          L2Client
	enclave     enclave.RPC
}

type Proposal struct {
	Output      *enclave.Proposal
	From        eth.L2BlockRef
	To          eth.L2BlockRef
	Withdrawals bool
}

func NewProver(
	ctx context.Context,
	l1 L1Client,
	l2 L2Client,
	rollup RollupClient,
	enclav enclave.RPC,
) (*Prover, error) {
	rollupConfig, err := rollup.RollupConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch rollup config: %w", err)
	}
	cfg := enclave.FromRollupConfig(rollupConfig)
	chainConfig, err := l2.ChainConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to fetch chain config: %w", err)
	}
	return &Prover{
		config:      cfg,
		chainConfig: chainConfig,
		configHash:  cfg.Hash(),
		l1:          l1,
		l2:          l2,
		enclave:     enclav,
	}, nil
}

func (o *Prover) Generate(ctx context.Context, block *types.Block) (*Proposal, error) {
	witnessCh := await(func() (*stateless.ExecutionWitness, error) {
		return o.l2.ExecutionWitness(ctx, block.Hash())
	}, func(err error) error {
		return fmt.Errorf("failed to fetch witness: %w", err)
	})

	messageAccountCh := await(func() (*eth.AccountResult, error) {
		return o.l2.GetProof(ctx, predeploys.L2ToL1MessagePasserAddr, block.Hash())
	}, func(err error) error {
		return fmt.Errorf("failed to fetch message account proof: %w", err)
	})

	previousBlockCh := await(func() (*types.Block, error) {
		return o.l2.BlockByHash(ctx, block.ParentHash())
	}, func(err error) error {
		return fmt.Errorf("failed to fetch previous L2 block: %w", err)
	})

	prevMessageAccountCh := await(func() (*eth.AccountResult, error) {
		return o.l2.GetProof(ctx, predeploys.L2ToL1MessagePasserAddr, block.ParentHash())
	}, func(err error) error {
		return fmt.Errorf("failed to fetch previous message account proof: %w", err)
	})

	blockRef, err := derive.L2BlockToBlockRef(o.config.ToRollupConfig(), block)
	if err != nil {
		return nil, fmt.Errorf("failed to derive block ref from L2 block: %w", err)
	}

	l1OriginCh := await(func() (*types.Header, error) {
		return o.l1.HeaderByHash(ctx, blockRef.L1Origin.Hash)
	}, func(err error) error {
		return fmt.Errorf("failed to fetch L1 origin header: %w", err)
	})

	l1ReceiptsCh := await(func() (types.Receipts, error) {
		return o.l1.BlockReceipts(ctx, blockRef.L1Origin.Hash)
	}, func(err error) error {
		return fmt.Errorf("failed to fetch L1 receipts: %w", err)
	})

	var errors []error

	witness := <-witnessCh
	errors = appendNonNil(errors, witness.err)

	messageAccount := <-messageAccountCh
	errors = appendNonNil(errors, messageAccount.err)

	previousBlock := <-previousBlockCh
	errors = appendNonNil(errors, previousBlock.err)

	l1Origin := <-l1OriginCh
	errors = appendNonNil(errors, l1Origin.err)

	l1Receipts := <-l1ReceiptsCh
	errors = appendNonNil(errors, l1Receipts.err)

	prevMessageAccount := <-prevMessageAccountCh
	errors = appendNonNil(errors, prevMessageAccount.err)

	if len(errors) > 0 {
		return nil, &multierror.Error{Errors: errors}
	}

	marshalTxs := func(txs types.Transactions, includeDeposits bool) ([]hexutil.Bytes, error) {
		var rlps []hexutil.Bytes
		for _, tx := range txs {
			if !includeDeposits && tx.IsDepositTx() {
				continue
			}
			rlp, err := tx.MarshalBinary()
			if err != nil {
				return nil, fmt.Errorf("failed to marshal transaction: %w", err)
			}
			rlps = append(rlps, rlp)
		}
		return rlps, nil
	}
	previousTxs, err := marshalTxs(previousBlock.value.Transactions(), true)
	if err != nil {
		return nil, err
	}
	sequencedTxs, err := marshalTxs(block.Transactions(), false)
	if err != nil {
		return nil, err
	}

	output, err := o.enclave.ExecuteStateless(
		ctx,
		o.config,
		o.chainConfig,
		l1Origin.value,
		l1Receipts.value,
		previousTxs,
		block.Header(),
		sequencedTxs,
		witness.value,
		messageAccount.value,
		prevMessageAccount.value.StorageHash,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to execute enclave state transition: %w", err)
	}
	if output.L1OriginHash != blockRef.L1Origin.Hash {
		return nil, fmt.Errorf("output L1 origin hash does not match expected: %s != %s", output.L1OriginHash, blockRef.L1Origin.Hash)
	}
	if output.L2BlockNumber.ToInt().Cmp(block.Number()) != 0 {
		return nil, fmt.Errorf("output L2 block number does not match expected: %s != %s", output.L2BlockNumber, block.Number())
	}

	outputRoot := enclave.OutputRootV0(block.Header(), messageAccount.value.StorageHash)
	if output.OutputRoot != outputRoot {
		return nil, fmt.Errorf("output root does not match expected: %s != %s", output.OutputRoot, outputRoot)
	}

	return &Proposal{
		Output:      output,
		From:        blockRef,
		To:          blockRef,
		Withdrawals: block.Bloom().Test(predeploys.L2ToL1MessagePasserAddr.Bytes()),
	}, nil
}

func (o *Prover) Aggregate(ctx context.Context, prevOutputRoot common.Hash, proposals []*Proposal) (*Proposal, error) {
	if len(proposals) == 0 {
		return nil, fmt.Errorf("no proposals to aggregate")
	}
	if len(proposals) == 1 {
		return proposals[0], nil
	}
	prop := make([]*enclave.Proposal, len(proposals))
	withdrawals := false
	for i, p := range proposals {
		prop[i] = p.Output
		withdrawals = withdrawals || p.Withdrawals
	}
	output, err := o.enclave.Aggregate(ctx, o.configHash, prevOutputRoot, prop)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate proposals: %w", err)
	}
	return &Proposal{
		Output:      output,
		From:        proposals[0].From,
		To:          proposals[len(proposals)-1].To,
		Withdrawals: withdrawals,
	}, nil
}

type result[E any] struct {
	value E
	err   error
}

func await[E any](f func() (E, error), w func(err error) error) chan result[E] {
	ch := make(chan result[E], 1)
	go func() {
		value, err := f()
		if err != nil {
			err = w(err)
		}
		ch <- result[E]{value, err}
	}()
	return ch
}

func appendNonNil(r []error, e error) []error {
	if e != nil {
		r = append(r, e)
	}
	return r
}
