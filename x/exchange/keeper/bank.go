package keeper

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

func (k Keeper) QueueSendCoins(ctx sdk.Context, fromAddr, toAddr sdk.AccAddress, amt sdk.Coins) error {
	for _, coin := range amt {
		balance := k.GetTransientBalance(ctx, fromAddr, coin.Denom)
		newBalance := balance.AddAmount(coin.Amount.Neg())
		if err := k.SetTransientBalance(ctx, fromAddr, newBalance); err != nil {
			return err
		}
		balance = k.GetTransientBalance(ctx, toAddr, coin.Denom)
		newBalance = balance.AddAmount(coin.Amount)
		if err := k.SetTransientBalance(ctx, toAddr, newBalance); err != nil {
			return err
		}
	}
	return nil
}

func (k Keeper) ExecuteSendCoins(ctx sdk.Context) error {
	var (
		inputs  []banktypes.Input
		outputs []banktypes.Output
	)
	k.IterateAllTransientBalances(ctx, func(addr sdk.AccAddress, coin sdk.Coin) (stop bool) {
		if coin.IsNegative() {
			inputs = append(
				inputs, banktypes.NewInput(addr, sdk.NewCoins(sdk.Coin{Denom: coin.Denom, Amount: coin.Amount.Neg()})))
		} else {
			outputs = append(
				outputs, banktypes.NewOutput(addr, sdk.NewCoins(coin)))
		}
		k.DeleteTransientBalance(ctx, addr, coin.Denom)
		return false
	})
	return k.bankKeeper.InputOutputCoins(ctx, inputs, outputs)
}
