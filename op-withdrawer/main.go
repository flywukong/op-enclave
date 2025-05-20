package main

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/base/op-enclave/bindings"
	"github.com/base/op-enclave/op-withdrawer/withdrawals"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	oplog "github.com/ethereum-optimism/optimism/op-service/log"
	"github.com/ethereum-optimism/optimism/op-service/opio"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/urfave/cli/v2"
)

var (
	L1URLFlag = &cli.StringFlag{
		Name:     "l1-url",
		Usage:    "URL of an L1 RPC host",
		EnvVars:  []string{"L1_URL"},
		Required: true,
	}
	L2URLFlag = &cli.StringFlag{
		Name:     "l2-url",
		Usage:    "URL of an L2 RPC host",
		EnvVars:  []string{"L2_URL"},
		Required: true,
	}
	PortalAddressFlag = &cli.StringFlag{
		Name:     "portal-address",
		Usage:    "Optimism Portal address",
		EnvVars:  []string{"PORTAL_ADDRESS"},
		Required: true,
	}
	PrivateKeyFlag = &cli.StringFlag{
		Name:     "private-key",
		Usage:    "Private key to sign the transaction",
		EnvVars:  []string{"PRIVATE_KEY"},
		Required: true,
	}
)

func main() {
	oplog.SetupDefaults()

	app := cli.NewApp()
	app.Name = "withdrawer"
	app.Usage = "Withdraws funds from L2 to L1"
	app.Action = func(c *cli.Context) error {
		return cli.ShowAppHelp(c)
	}
	app.Commands = []*cli.Command{
		{
			Name:   "depositHash",
			Usage:  "Calculate L2 deposit hash(es) from L1 deposit tx",
			Action: DepositHash,
			Flags: []cli.Flag{
				L1URLFlag,
				PortalAddressFlag,
			},
		},
		{
			Name:   "proveWithdrawal",
			Usage:  "Prove and finalize L2 -> L1 withdrawal(s)",
			Action: Main,
			Flags: []cli.Flag{
				L1URLFlag,
				L2URLFlag,
				PortalAddressFlag,
				PrivateKeyFlag,
			},
		},
	}

	ctx := opio.WithSignalWaiterMain(context.Background())
	err := app.RunContext(ctx, os.Args)
	if err != nil {
		log.Crit("Application failed", "message", err)
	}
}

func Main(cliCtx *cli.Context) error {
	l1URL := cliCtx.String(L1URLFlag.Name)
	l2URL := cliCtx.String(L2URLFlag.Name)
	portalAddress := common.HexToAddress(cliCtx.String(PortalAddressFlag.Name))
	privateKey, err := crypto.ToECDSA(common.FromHex(cliCtx.String(PrivateKeyFlag.Name)))
	if err != nil {
		return err
	}

	withdrawalTxHash := common.HexToHash(cliCtx.Args().First())
	if (withdrawalTxHash == common.Hash{}) {
		return fmt.Errorf("invalid withdrawal transaction hash")
	}

	ctx := context.Background()
	l1, err := ethclient.DialContext(ctx, l1URL)
	if err != nil {
		return err
	}
	l2, err := ethclient.DialContext(ctx, l2URL)
	if err != nil {
		return err
	}
	l2g := gethclient.New(l2.Client())

	chainID, err := l1.ChainID(ctx)
	if err != nil {
		return err
	}
	opts, err := bind.NewKeyedTransactorWithChainID(privateKey, chainID)
	if err != nil {
		return err
	}

	portal, err := bindings.NewPortal(portalAddress, l1)
	if err != nil {
		return err
	}

	receipt, err := withdrawals.WaitForReceipt(ctx, l2, withdrawalTxHash, 1*time.Second)
	if err != nil {
		return err
	}

	receipts, err := ProveWithdrawal(ctx, l1, l2, l2g, opts, portal, withdrawalTxHash, receipt.BlockNumber)
	if err != nil {
		return err
	}

	for _, receipt := range receipts {
		fmt.Printf("Withdrawal proved and finalized: %s\n", receipt.TxHash)
	}

	return nil
}

func ProveWithdrawal(ctx context.Context, l1, l2 *ethclient.Client, l2g *gethclient.Client, opts *bind.TransactOpts, portal *bindings.Portal, withdrawalTxHash common.Hash, withdrawalTxBlock *big.Int) ([]*types.Receipt, error) {
	pollInterval := 1 * time.Second

	outputOracleAddress, err := portal.L2Oracle(&bind.CallOpts{})
	if err != nil {
		return nil, err
	}
	outputOracle, err := bindings.NewL2OutputOracle(outputOracleAddress, l1)
	if err != nil {
		return nil, err
	}

	fmt.Printf("Waiting for TEE proof of block %d... ", withdrawalTxBlock)
	l2OutputBlock, err := withdrawals.WaitForOutputBlock(ctx, outputOracle, withdrawalTxBlock, pollInterval)
	fmt.Println("done")

	txs, err := withdrawals.ProveAndFinalizeWithdrawals(ctx, l2g, l2, opts, outputOracle, portal, withdrawalTxHash, l2OutputBlock)
	if err != nil {
		return nil, err
	}

	receipts := make([]*types.Receipt, len(txs))
	for i, tx := range txs {
		receipts[i], err = withdrawals.WaitForReceipt(ctx, l1, tx.Hash(), pollInterval)
		if err != nil {
			return nil, err
		}
	}
	return receipts, nil
}

func DepositHash(cliCtx *cli.Context) error {
	l1URL := cliCtx.String(L1URLFlag.Name)
	portalAddr := common.HexToAddress(cliCtx.String(PortalAddressFlag.Name))

	ctx := context.Background()
	l1, err := ethclient.DialContext(ctx, l1URL)
	if err != nil {
		return err
	}

	depositTxHash := common.HexToHash(cliCtx.Args().First())
	if (depositTxHash == common.Hash{}) {
		return fmt.Errorf("invalid deposit transaction hash")
	}

	receipt, err := l1.TransactionReceipt(ctx, depositTxHash)
	if err != nil {
		return err
	}

	deposits, err := derive.UserDeposits([]*types.Receipt{receipt}, portalAddr)
	if err != nil {
		return err
	}
	if len(deposits) == 0 {
		return fmt.Errorf("no deposits found")
	}

	// a single tx can contain multiple deposits; print them all
	for _, deposit := range deposits {
		fmt.Println(types.NewTx(deposit).Hash().Hex())
	}

	return nil
}
