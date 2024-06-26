// Copyright (c) 2024 Blockwatch Data Inc.
// Author: alex@blockwatch.cc

package rpc

import (
	"github.com/mavryk-network/mvgo/mavryk"
	"github.com/mavryk-network/mvgo/micheline"
)

// Ensure TransferTicket implements the TypedOperation interface.
var _ TypedOperation = (*TransferTicket)(nil)

type TransferTicket struct {
	Manager
	Destination mavryk.Address `json:"destination"`
	Entrypoint  string         `json:"entrypoint"`
	Type        micheline.Prim `json:"ticket_ty"`
	Contents    micheline.Prim `json:"ticket_contents"`
	Ticketer    mavryk.Address `json:"ticket_ticketer"`
	Amount      mavryk.Z       `json:"ticket_amount"`
}

// Costs returns operation cost to implement TypedOperation interface.
func (t TransferTicket) Costs() mavryk.Costs {
	res := t.Metadata.Result
	cost := mavryk.Costs{
		Fee:     t.Manager.Fee,
		GasUsed: res.Gas(),
	}
	if !t.Result().IsSuccess() {
		return cost
	}
	for _, v := range res.BalanceUpdates {
		if v.Kind != CONTRACT {
			continue
		}
		burn := v.Amount()
		if burn >= 0 {
			continue
		}
		cost.StorageBurn += -burn
	}
	return cost
}

func (t TransferTicket) EncodeParameters() micheline.Prim {
	return micheline.NewPair(
		micheline.NewBytes(t.Ticketer.EncodePadded()), // ticketer
		micheline.NewPair(
			t.Type, // type
			micheline.NewPair(
				t.Contents,                       // contents
				micheline.NewNat(t.Amount.Big()), // amount
			),
		),
	)
}

// Addresses adds all addresses used in this operation to the set.
// Implements TypedOperation interface.
func (t TransferTicket) Addresses(set *mavryk.AddressSet) {
	set.AddUnique(t.Source)
	set.AddUnique(t.Destination)
	set.AddUnique(t.Ticketer)
}

func (t TransferTicket) AddEmbeddedAddresses(addUnique func(mavryk.Address)) {
	if !t.Destination.IsContract() {
		return
	}
	collect := func(p micheline.Prim) error {
		switch {
		case len(p.String) == 36 || len(p.String) == 37:
			if a, err := mavryk.ParseAddress(p.String); err == nil {
				addUnique(a)
			}
			return micheline.PrimSkip
		case mavryk.IsAddressBytes(p.Bytes):
			a := mavryk.Address{}
			if err := a.Decode(p.Bytes); err == nil {
				addUnique(a)
			}
			return micheline.PrimSkip
		default:
			return nil
		}
	}

	// from storage
	_ = t.Metadata.Result.Storage.Walk(collect)

	// from bigmap updates
	for _, v := range t.Metadata.Result.BigmapEvents() {
		if v.Action != micheline.DiffActionUpdate {
			continue
		}
		_ = v.Key.Walk(collect)
		_ = v.Value.Walk(collect)
	}

	// from ticket updates
	for _, it := range t.Metadata.Result.TicketUpdates() {
		for _, v := range it.Updates {
			addUnique(v.Account)
		}
	}

	// from internal results
	for _, it := range t.Metadata.InternalResults {
		if it.Script != nil {
			_ = it.Script.Storage.Walk(collect)
		}
		_ = it.Result.Storage.Walk(collect)
		for _, v := range it.Result.BigmapEvents() {
			if v.Action != micheline.DiffActionUpdate {
				continue
			}
			_ = v.Key.Walk(collect)
			_ = v.Value.Walk(collect)
		}
		// from ticket updates
		for _, v := range it.Result.TicketUpdates() {
			for _, vv := range v.Updates {
				addUnique(vv.Account)
			}
		}
	}
}
