// Copyright (c) 2020-2024 Blockwatch Data Inc.
// Author: alex@blockwatch.cc

package etl

import (
	"github.com/mavryk-network/mvindex/etl/model"
	"github.com/mavryk-network/mvindex/rpc"
)

func (b *Builder) NewActivationFlow(acc *model.Account, aop *rpc.Activation, id model.OpRef) []*model.Flow {
	bal := aop.Fees()
	if len(bal) < 1 {
		log.Warnf("Empty balance update for activation op at height %d", b.block.Height)
	}
	f := model.NewFlow(b.block, acc, nil, id)
	f.Kind = model.FlowKindBalance
	f.Type = model.FlowTypeActivation
	for _, u := range bal {
		if u.Kind == "contract" {
			f.AmountIn = u.Amount()
		}
	}
	b.block.Flows = append(b.block.Flows, f)
	return []*model.Flow{f}
}
