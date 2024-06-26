// Copyright (c) 2020-2024 Blockwatch Data Inc.
// Author: alex@blockwatch.cc

package etl

import (
	"github.com/mavryk-network/mvindex/etl/model"
	"github.com/mavryk-network/mvindex/rpc"
)

// Fees for manager operations (optional, i.e. sender may set to zero,
// for batch ops fees may also be paid by any op in batch)
func (b *Builder) NewFeeFlows(src *model.Account, fees rpc.BalanceUpdates, id model.OpRef) ([]*model.Flow, int64) {
	var sum int64
	flows := make([]*model.Flow, 0)
	typ := model.MapFlowType(id.Kind)
	for _, u := range fees {
		if u.Change == 0 {
			continue
		}
		switch u.Kind {
		case "contract":
			// pre/post-Ithaca fees paid by src
			f := model.NewFlow(b.block, src, b.block.Proposer.Account, id)
			f.Kind = model.FlowKindBalance
			f.Type = typ
			f.AmountOut = -u.Change // note the negation!
			f.IsFee = true
			sum += -u.Change
			flows = append(flows, f)
		case "freezer":
			// pre-Ithaca: fees paid to baker
			if u.Category == "fees" {
				f := model.NewFlow(b.block, b.block.Proposer.Account, src, id)
				f.Kind = model.FlowKindFees
				f.Type = typ
				f.AmountIn = u.Change
				f.IsFrozen = true
				flows = append(flows, f)
			}
			// case "accumulator":
			// post-Ithaca: unused
		}
	}

	// delegation change is handled outside
	return flows, sum
}
