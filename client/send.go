package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	arkv1 "github.com/ark-network/ark/api-spec/protobuf/gen/ark/v1"
	"github.com/ark-network/ark/common"
	"github.com/urfave/cli/v2"
	"github.com/vulpemventures/go-elements/address"
	"github.com/vulpemventures/go-elements/psetv2"
)

type receiver struct {
	To     string `json:"to"`
	Amount uint64 `json:"amount"`
}

func (r *receiver) isOnchain() bool {
	_, err := address.ToOutputScript(r.To)
	return err == nil
}

var (
	receiversFlag = cli.StringFlag{
		Name:  "receivers",
		Usage: "receivers of the send transaction, JSON encoded: '[{\"to\": \"<...>\", \"amount\": <...>}, ...]'",
	}
	toFlag = cli.StringFlag{
		Name:  "to",
		Usage: "address of the recipient",
	}
	amountFlag = cli.Uint64Flag{
		Name:  "amount",
		Usage: "amount to send in sats",
	}
	enableExpiryCoinselectFlag = cli.BoolFlag{
		Name:  "enable-expiry-coinselect",
		Usage: "select vtxos that are about to expire first",
		Value: false,
	}
)

var sendCommand = cli.Command{
	Name:   "send",
	Usage:  "Send your onchain or offchain funds to one or many receivers",
	Action: sendAction,
	Flags:  []cli.Flag{&receiversFlag, &toFlag, &amountFlag, &passwordFlag, &enableExpiryCoinselectFlag},
}

func sendAction(ctx *cli.Context) error {
	if !ctx.IsSet("receivers") && !ctx.IsSet("to") && !ctx.IsSet("amount") {
		return fmt.Errorf("missing destination, either use --to and --amount to send or --receivers to send to many")
	}
	receivers := ctx.String("receivers")
	to := ctx.String("to")
	amount := ctx.Uint64("amount")

	var receiversJSON []receiver
	if len(receivers) > 0 {
		if err := json.Unmarshal([]byte(receivers), &receiversJSON); err != nil {
			return fmt.Errorf("invalid receivers: %s", err)
		}
	} else {
		receiversJSON = []receiver{
			{
				To:     to,
				Amount: amount,
			},
		}
	}

	if len(receiversJSON) <= 0 {
		return fmt.Errorf("no receivers specified")
	}

	onchainReceivers := make([]receiver, 0)
	offchainReceivers := make([]receiver, 0)

	for _, receiver := range receiversJSON {
		if receiver.isOnchain() {
			onchainReceivers = append(onchainReceivers, receiver)
		} else {
			offchainReceivers = append(offchainReceivers, receiver)
		}
	}

	explorer := NewExplorer(ctx)

	if len(onchainReceivers) > 0 {
		pset, err := sendOnchain(ctx, onchainReceivers)
		if err != nil {
			return err
		}

		txid, err := explorer.Broadcast(pset)
		if err != nil {
			return err
		}

		return printJSON(map[string]interface{}{
			"txid": txid,
		})
	}

	if len(offchainReceivers) > 0 {
		if err := sendOffchain(ctx, offchainReceivers); err != nil {
			return err
		}
	}

	return nil
}

func sendOffchain(ctx *cli.Context, receivers []receiver) error {
	withExpiryCoinselect := ctx.Bool("enable-expiry-coinselect")

	offchainAddr, _, _, err := getAddress(ctx)
	if err != nil {
		return err
	}

	_, _, aspPubKey, err := common.DecodeAddress(offchainAddr)
	if err != nil {
		return err
	}

	receiversOutput := make([]*arkv1.Output, 0)
	sumOfReceivers := uint64(0)

	for _, receiver := range receivers {
		_, _, aspKey, err := common.DecodeAddress(receiver.To)
		if err != nil {
			return fmt.Errorf("invalid receiver address: %s", err)
		}

		if !bytes.Equal(
			aspPubKey.SerializeCompressed(), aspKey.SerializeCompressed(),
		) {
			return fmt.Errorf("invalid receiver address '%s': must be associated with the connected service provider", receiver.To)
		}

		if receiver.Amount < DUST {
			return fmt.Errorf("invalid amount (%d), must be greater than dust %d", receiver.Amount, DUST)
		}

		receiversOutput = append(receiversOutput, &arkv1.Output{
			Address: receiver.To,
			Amount:  uint64(receiver.Amount),
		})
		sumOfReceivers += receiver.Amount
	}
	client, close, err := getClientFromState(ctx)
	if err != nil {
		return err
	}
	defer close()

	explorer := NewExplorer(ctx)

	vtxos, err := getVtxos(ctx, explorer, client, offchainAddr, withExpiryCoinselect)
	if err != nil {
		return err
	}
	selectedCoins, changeAmount, err := coinSelect(vtxos, sumOfReceivers, withExpiryCoinselect)
	if err != nil {
		return err
	}

	if changeAmount > 0 {
		changeReceiver := &arkv1.Output{
			Address: offchainAddr,
			Amount:  changeAmount,
		}
		receiversOutput = append(receiversOutput, changeReceiver)
	}

	inputs := make([]*arkv1.Input, 0, len(selectedCoins))

	for _, coin := range selectedCoins {
		inputs = append(inputs, &arkv1.Input{
			Txid: coin.txid,
			Vout: coin.vout,
		})
	}

	secKey, err := privateKeyFromPassword(ctx)
	if err != nil {
		return err
	}

	registerResponse, err := client.RegisterPayment(
		ctx.Context, &arkv1.RegisterPaymentRequest{Inputs: inputs},
	)
	if err != nil {
		return err
	}

	_, err = client.ClaimPayment(ctx.Context, &arkv1.ClaimPaymentRequest{
		Id:      registerResponse.GetId(),
		Outputs: receiversOutput,
	})
	if err != nil {
		return err
	}

	poolTxID, err := handleRoundStream(
		ctx, client, registerResponse.GetId(),
		selectedCoins, secKey, receiversOutput,
	)
	if err != nil {
		return err
	}

	return printJSON(map[string]interface{}{
		"pool_txid": poolTxID,
	})
}

func sendOnchain(ctx *cli.Context, receivers []receiver) (string, error) {
	pset, err := psetv2.New(nil, nil, nil)
	if err != nil {
		return "", err
	}
	updater, err := psetv2.NewUpdater(pset)
	if err != nil {
		return "", err
	}

	_, net := getNetwork(ctx)

	targetAmount := uint64(0)
	for _, receiver := range receivers {
		targetAmount += receiver.Amount
		if receiver.Amount < DUST {
			return "", fmt.Errorf("invalid amount (%d), must be greater than dust %d", receiver.Amount, DUST)
		}

		script, err := address.ToOutputScript(receiver.To)
		if err != nil {
			return "", err
		}

		if err := updater.AddOutputs([]psetv2.OutputArgs{
			{
				Asset:  net.AssetID,
				Amount: receiver.Amount,
				Script: script,
			},
		}); err != nil {
			return "", err
		}
	}

	explorer := NewExplorer(ctx)

	utxos, delayedUtxos, change, err := coinSelectOnchain(
		ctx, explorer, targetAmount, nil,
	)
	if err != nil {
		return "", err
	}

	if err := addInputs(ctx, updater, utxos, delayedUtxos, net); err != nil {
		return "", err
	}

	if change > 0 {
		_, changeAddr, _, err := getAddress(ctx)
		if err != nil {
			return "", err
		}

		changeScript, err := address.ToOutputScript(changeAddr)
		if err != nil {
			return "", err
		}

		if err := updater.AddOutputs([]psetv2.OutputArgs{
			{
				Asset:  net.AssetID,
				Amount: change,
				Script: changeScript,
			},
		}); err != nil {
			return "", err
		}
	}

	utx, err := pset.UnsignedTx()
	if err != nil {
		return "", err
	}

	vBytes := utx.VirtualSize()
	feeAmount := uint64(math.Ceil(float64(vBytes) * 0.5))

	if change > feeAmount {
		updater.Pset.Outputs[len(updater.Pset.Outputs)-1].Value = change - feeAmount
	} else if change == feeAmount {
		updater.Pset.Outputs = updater.Pset.Outputs[:len(updater.Pset.Outputs)-1]
	} else { // change < feeAmount
		if change > 0 {
			updater.Pset.Outputs = updater.Pset.Outputs[:len(updater.Pset.Outputs)-1]
		}
		// reselect the difference
		selected, delayedSelected, newChange, err := coinSelectOnchain(
			ctx, explorer, feeAmount-change, append(utxos, delayedUtxos...),
		)
		if err != nil {
			return "", err
		}

		if err := addInputs(ctx, updater, selected, delayedSelected, net); err != nil {
			return "", err
		}

		if newChange > 0 {
			_, changeAddr, _, err := getAddress(ctx)
			if err != nil {
				return "", err
			}

			changeScript, err := address.ToOutputScript(changeAddr)
			if err != nil {
				return "", err
			}

			if err := updater.AddOutputs([]psetv2.OutputArgs{
				{
					Asset:  net.AssetID,
					Amount: newChange,
					Script: changeScript,
				},
			}); err != nil {
				return "", err
			}
		}
	}

	if err := updater.AddOutputs([]psetv2.OutputArgs{
		{
			Asset:  net.AssetID,
			Amount: feeAmount,
		},
	}); err != nil {
		return "", err
	}

	prvKey, err := privateKeyFromPassword(ctx)
	if err != nil {
		return "", err
	}

	if err := signPset(ctx, updater.Pset, explorer, prvKey); err != nil {
		return "", err
	}

	if err := psetv2.FinalizeAll(updater.Pset); err != nil {
		return "", err
	}

	return updater.Pset.ToBase64()
}
