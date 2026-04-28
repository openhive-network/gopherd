package chain

import "hived-go/internal/protocol/asset"

// hiveAsset builds an asset.Asset for `amount` HIVE in legacy encoding.
func hiveAsset(amount int64) asset.Asset {
	return asset.Asset{Amount: amount, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}}
}
