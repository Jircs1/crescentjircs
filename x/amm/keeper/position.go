package keeper

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	utils "github.com/crescent-network/crescent/v5/types"
	"github.com/crescent-network/crescent/v5/x/amm/types"
	exchangetypes "github.com/crescent-network/crescent/v5/x/exchange/types"
)

func (k Keeper) AddLiquidity(
	ctx sdk.Context, ownerAddr sdk.AccAddress, poolId uint64,
	lowerPrice, upperPrice sdk.Dec, desiredAmt sdk.Coins) (position types.Position, liquidity sdk.Dec, amt sdk.Coins, err error) {
	lowerTick, valid := exchangetypes.ValidateTickPrice(lowerPrice, TickPrecision)
	if !valid {
		err = sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "invalid lower tick")
		return
	}
	upperTick, valid := exchangetypes.ValidateTickPrice(upperPrice, TickPrecision)
	if !valid {
		err = sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "invalid upper tick")
		return
	}

	pool, found := k.GetPool(ctx, poolId)
	if !found {
		err = sdkerrors.Wrap(sdkerrors.ErrNotFound, "pool not found")
		return
	}
	desiredAmt0, desiredAmt1 := desiredAmt.AmountOf(pool.Denom0), desiredAmt.AmountOf(pool.Denom1)
	if lowerTick%int32(pool.TickSpacing) != 0 {
		err = sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "lower tick must be multiple of tick spacing")
		return
	}
	if upperTick%int32(pool.TickSpacing) != 0 {
		err = sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "upper tick must be multiple of tick spacing")
		return
	}
	poolState := k.MustGetPoolState(ctx, poolId)

	sqrtPriceA := types.SqrtPriceAtTick(lowerTick, TickPrecision) // TODO: use tick prec param
	sqrtPriceB := types.SqrtPriceAtTick(upperTick, TickPrecision) // TODO: use tick prec param
	liquidity = types.LiquidityForAmounts(
		utils.DecApproxSqrt(poolState.CurrentPrice), sqrtPriceA, sqrtPriceB, desiredAmt0, desiredAmt1)

	var amt0, amt1 sdk.Int
	position, amt0, amt1 = k.modifyPosition(
		ctx, pool, ownerAddr, lowerTick, upperTick, liquidity)

	depositCoins := sdk.NewCoins(sdk.NewCoin(pool.Denom0, amt0), sdk.NewCoin(pool.Denom1, amt1))
	if err = k.bankKeeper.SendCoins(
		ctx, ownerAddr, sdk.MustAccAddressFromBech32(pool.ReserveAddress), depositCoins); err != nil {
		return
	}
	// TODO: emit event
	return
}

func (k Keeper) RemoveLiquidity(
	ctx sdk.Context, ownerAddr sdk.AccAddress, positionId uint64, liquidity sdk.Dec) (position types.Position, amt sdk.Coins, err error) {
	var found bool
	position, found = k.GetPosition(ctx, positionId)
	if !found {
		err = sdkerrors.Wrap(sdkerrors.ErrNotFound, "position not found")
		return
	}
	if ownerAddr.String() != position.Owner {
		err = sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "position is not owned by the user")
		return
	}
	if position.Liquidity.LT(liquidity) {
		err = sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "position liquidity is smaller than the liquidity specified")
		return
	}

	pool, found := k.GetPool(ctx, position.PoolId)
	if !found { // sanity check
		panic("pool not found")
	}

	var amt0, amt1 sdk.Int
	position, amt0, amt1 = k.modifyPosition(
		ctx, pool, ownerAddr, position.LowerTick, position.UpperTick, liquidity.Neg())
	amt0, amt1 = amt0.Neg(), amt1.Neg()
	if amt0.IsPositive() || amt1.IsPositive() {
		position.OwedToken0 = position.OwedToken0.Add(amt0)
		position.OwedToken1 = position.OwedToken1.Add(amt1)
		k.SetPosition(ctx, position)
	}

	withdrawCoins := sdk.NewCoins(sdk.NewCoin(pool.Denom0, amt0), sdk.NewCoin(pool.Denom1, amt1))
	if withdrawCoins.IsAllPositive() {
		if err = k.bankKeeper.SendCoins(
			ctx, sdk.MustAccAddressFromBech32(pool.ReserveAddress), ownerAddr, withdrawCoins); err != nil {
			return
		}
	}
	return
}

func (k Keeper) Collect(
	ctx sdk.Context, ownerAddr sdk.AccAddress, positionId uint64, amt sdk.Coins) error {
	position, found := k.GetPosition(ctx, positionId)
	if !found {
		return sdkerrors.Wrap(sdkerrors.ErrNotFound, "position not found")
	}
	if ownerAddr.String() != position.Owner {
		return sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "position is not owned by the user")
	}

	position, _, err := k.RemoveLiquidity(ctx, ownerAddr, positionId, utils.ZeroDec)
	if err != nil {
		return err
	}
	pool, found := k.GetPool(ctx, position.PoolId)
	if !found { // sanity check
		panic("pool not found")
	}
	collectible := sdk.NewCoins(
		sdk.NewCoin(pool.Denom0, position.OwedToken0),
		sdk.NewCoin(pool.Denom1, position.OwedToken1)).
		Add(position.OwedFarmingRewards...)
	if !collectible.IsAllGTE(amt) {
		return sdkerrors.Wrapf(
			sdkerrors.ErrInsufficientFunds, "collectible %s is smaller than %s", collectible, amt)
	}
	amt0 := utils.MinInt(position.OwedToken0, amt.AmountOf(pool.Denom0))
	amt1 := utils.MinInt(position.OwedToken1, amt.AmountOf(pool.Denom1))
	position.OwedToken0 = position.OwedToken0.Sub(amt0)
	position.OwedToken1 = position.OwedToken1.Sub(amt1)
	fees := sdk.NewCoins(sdk.NewCoin(pool.Denom0, amt0), sdk.NewCoin(pool.Denom1, amt1))
	// TODO: use lp fee address
	if err := k.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, ownerAddr, fees); err != nil {
		return err
	}
	amt = amt.Sub(fees)
	position.OwedFarmingRewards = position.OwedFarmingRewards.Sub(amt)
	if err := k.bankKeeper.SendCoins(ctx, types.RewardsPoolAddress, ownerAddr, amt); err != nil {
		return err
	}
	k.SetPosition(ctx, position)

	return nil
}

func (k Keeper) modifyPosition(
	ctx sdk.Context, pool types.Pool, ownerAddr sdk.AccAddress,
	lowerTick, upperTick int32, liquidityDelta sdk.Dec) (position types.Position, amt0, amt1 sdk.Int) {
	// TODO: validate ticks
	var found bool
	position, found = k.GetPositionByParams(ctx, ownerAddr, pool.Id, lowerTick, upperTick)
	if !found {
		positionId := k.GetNextPositionIdWithUpdate(ctx)
		position = types.NewPosition(positionId, pool.Id, ownerAddr, lowerTick, upperTick)
		k.SetPositionByParamsIndex(ctx, position)
	}

	if liquidityDelta.IsZero() && !position.Liquidity.IsPositive() { // sanity check
		panic("cannot poke zero liquidity positions")
	}

	// begin _updatePosition()
	poolState := k.MustGetPoolState(ctx, pool.Id)
	var flippedLower, flippedUpper bool
	if !liquidityDelta.IsZero() {
		flippedLower = k.updateTick(
			ctx, pool.Id, lowerTick, poolState.CurrentTick, liquidityDelta,
			poolState.FeeGrowthGlobal0, poolState.FeeGrowthGlobal1, false)
		flippedUpper = k.updateTick(
			ctx, pool.Id, upperTick, poolState.CurrentTick, liquidityDelta,
			poolState.FeeGrowthGlobal0, poolState.FeeGrowthGlobal1, true)
	}

	// TODO: optimize GetTickInfo
	feeGrowthInside0, feeGrowthInside1 := k.feeGrowthInside(
		ctx, pool.Id, lowerTick, upperTick, poolState.CurrentTick,
		poolState.FeeGrowthGlobal0, poolState.FeeGrowthGlobal1)
	farmingRewardsGrowthInside := k.farmingRewardsGrowthInside(
		ctx, pool.Id, lowerTick, upperTick, poolState.CurrentTick,
		poolState.FarmingRewardsGrowthGlobal)

	owedTokens0 := feeGrowthInside0.Sub(position.LastFeeGrowthInside0).MulTruncate(position.Liquidity).TruncateInt()
	owedTokens1 := feeGrowthInside1.Sub(position.LastFeeGrowthInside1).MulTruncate(position.Liquidity).TruncateInt()
	owedFarmingRewards, _ := farmingRewardsGrowthInside.Sub(position.LastFarmingRewardsGrowthInside).
		MulDecTruncate(position.Liquidity).TruncateDecimal()

	position.Liquidity = position.Liquidity.Add(liquidityDelta)
	position.LastFeeGrowthInside0 = feeGrowthInside0
	position.LastFeeGrowthInside1 = feeGrowthInside1
	position.OwedToken0 = position.OwedToken0.Add(owedTokens0)
	position.OwedToken1 = position.OwedToken1.Add(owedTokens1)
	position.LastFarmingRewardsGrowthInside = farmingRewardsGrowthInside
	position.OwedFarmingRewards = position.OwedFarmingRewards.Add(owedFarmingRewards...)
	k.SetPosition(ctx, position)

	if liquidityDelta.IsNegative() {
		if flippedLower {
			k.DeleteTickInfo(ctx, pool.Id, lowerTick)
		}
		if flippedUpper {
			k.DeleteTickInfo(ctx, pool.Id, upperTick)
		}
	}
	// end _updatePosition()

	// TODO: handle prec param and error correctly
	amt0 = utils.ZeroInt
	amt1 = utils.ZeroInt
	if !liquidityDelta.IsZero() {
		sqrtPriceA := types.SqrtPriceAtTick(lowerTick, TickPrecision)
		sqrtPriceB := types.SqrtPriceAtTick(upperTick, TickPrecision)
		if poolState.CurrentTick < lowerTick {
			amt0 = types.Amount0Delta(sqrtPriceA, sqrtPriceB, liquidityDelta)
		} else if poolState.CurrentTick < upperTick {
			currentSqrtPrice := utils.DecApproxSqrt(poolState.CurrentPrice)
			amt0 = types.Amount0Delta(currentSqrtPrice, sqrtPriceB, liquidityDelta)
			amt1 = types.Amount1Delta(sqrtPriceA, currentSqrtPrice, liquidityDelta)
			poolState.CurrentLiquidity = poolState.CurrentLiquidity.Add(liquidityDelta)
			k.SetPoolState(ctx, pool.Id, poolState)
		} else {
			amt1 = types.Amount1Delta(sqrtPriceA, sqrtPriceB, liquidityDelta)
		}
	}
	return
}
