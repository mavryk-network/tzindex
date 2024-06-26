// Copyright (c) 2020-2024 Blockwatch Data Inc.
// Author: alex@blockwatch.cc

package metadata

import "github.com/mavryk-network/mvgo/mavryk"

func init() {
	LoadSchema(payoutNs, []byte(payoutSchema), &Payout{})
}

const (
	payoutNs     = "payout"
	payoutSchema = `{
	"$schema": "http://json-schema.org/draft/2019-09/schema#",
	"$id": "https://api.mvpro.io/metadata/schemas/payout.json",
	"title": "Payout Info",
    "description": "List of Tezos baker addresses this payout address is related to.",
	"type": "array",
	"uniqueItems": true,
	"minItems": 1,
	"items": {
		"type": "string",
		"format": "tzaddress"
	}
}`
)

// reference to baker
type Payout []mavryk.Address

func (d Payout) Namespace() string {
	return payoutNs
}

func (d Payout) Validate() error {
	s, ok := GetSchema(payoutNs)
	if ok {
		return s.Validate(d)
	}
	return nil
}
