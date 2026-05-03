package testfixture

import (
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
)

// witnessUpdateOp returns a minimal witness_update operation for the
// named account with a deterministic block-signing key based on the
// account name. Used by tests that need a registered witness without
// caring about the specific signing key.
func witnessUpdateOp(owner string) op.WitnessUpdate {
	keySeed := byte(0)
	if len(owner) > 0 {
		keySeed = owner[0]
	}
	return op.WitnessUpdate{
		Owner:           owner,
		URL:             "x",
		BlockSigningKey: [33]byte{0x02, 1, keySeed},
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
		Fee: HIVE(0),
	}
}

// feedPublishOp returns a feed_publish operation for the named publisher
// and exchange rate.
func feedPublishOp(publisher string, rate asset.Price) op.FeedPublish {
	return op.FeedPublish{
		Publisher:    publisher,
		ExchangeRate: rate,
	}
}
