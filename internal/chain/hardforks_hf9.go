package chain

import (
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HF9 mass-rotates the owner/active/posting authorities of 150
// previously-compromised accounts to a single hived-controlled key.
// Mirrors database_hardfork.cpp:212-229 + the list at
// libraries/protocol/hardfork.d/0_9.hf:19-169.
//
// The post-rotation key is the public key for the well-known WIF
// `5JKopNRDhSqQQNc24KXLFmBz9YzfNX8oCN1Bj9Z7iLkKNRb2NRm` (per upstream
// 0_9.hf). The base58-prefixed form below is what hived uses on the
// wire.
const hf9CompromisedKeyB58 = "STM7sw22HqsXbz7D2CmJfmMwt9rimtk518dRzsR1f8Cgw52dQR1pR"

// hf9CompromisedAccounts is the verbatim sorted list from
// libraries/protocol/hardfork.d/0_9.hf:19-169 — 150 entries.
var hf9CompromisedAccounts = []string{
	"aeico", "aenor", "aizensou", "albertogm", "alibaba", "alyssas",
	"amartinezque", "amazon", "animalrobot", "arsahk", "arwani-wawan",
	"auxon", "b0y2k", "binwah-de-rese", "bitcube", "blackjincrypto",
	"boatymcboatface", "boy", "bravenewcoin", "bryner", "btcturbo",
	"bu328281", "bue", "bue-witness", "bunny", "cheftony", "chhayll",
	"chitty", "chryspano", "citibank", "cmtzco", "coinbitgold",
	"coinfund", "complexring", "cyrano.witness3", "dahaz159", "dan",
	"dantheman", "daycrypter", "dragonslayer109", "dulila", "ebay",
	"elyaque", "estin", "expanse", "fanie-vanhoten", "felipemachado",
	"fex", "fishayley", "fminerten1", "gabbans", "gatoso", "gazm",
	"gottahaveit", "graavor", "gregm", "grumpymutt", "gtg", "hcf27",
	"hello", "hipster", "hsbc", "ibnu", "ihashfury", "ikigai", "jacor",
	"james1337", "jamie", "john-kimmel", "jpmorgan", "justiciar",
	"kaptainkrayola", "karen13", "kevinwong", "kingofchaos", "lighthil",
	"linouxis9", "loewan", "luiz-marchi", "lux", "marcelhattingh",
	"mathiasbaer", "mauricemikkers", "mexbit", "mikemiziner", "mldorton",
	"mynameisbrian", "ned", "ned-scott", "news", "nicolaswsk", "norbu",
	"nuno-nutcrusherz", "omarb", "omarbitcoin", "opengas",
	"ossama-benjohn", "owdy", "ozmaster", "ozzy-vega", "pal",
	"penambang", "pierregi", "pwlaslo", "qamarpinkpanda", "recursive",
	"reddit2steem", "rimantas", "rok-sivante", "rseixas", "samuel-stone",
	"samupaha", "schro", "sebastien", "signalandnoise", "simoneighties",
	"slocum", "softbank", "sonzweil", "spartako", "stan", "steem-id",
	"steemit1", "steemitblog", "steempower", "steemychicken1",
	"str11ngfello", "streemian", "streetstyle", "summon", "teatree",
	"techemist", "the-alien", "thecryptodrive", "thegoodguy", "tonykent",
	"top10", "trevonjb", "troller", "trung81", "ukon", "val", "vippero",
	"walmart", "windsok", "wingz", "world", "worldfamous",
	"yan-kovalenko", "zebbra2014", "zer0sum",
}

// applyHardfork9CompromisedAccounts patches every compromised account
// that exists in state, rotating its owner/active/posting authorities
// to the HF9 key and writing an owner_authority_history row so post-HF9
// recover_account ops can match against the old owner authority.
//
// Block 3,165,946 created `slocum` with a specific owner key; HF9 at
// block ~3,202,773 rotated it. Two weeks later a recover_account op
// (block 3,333,790) submits the ORIGINAL pre-HF9 owner as
// `recent_owner_authority` — that match only succeeds if the HF9
// rotation left an owner_authority_history row behind. C++ does this;
// Go was skipping the entire HF9 step.
func applyHardfork9CompromisedAccounts(s *state.Session) {
	hf9Key, err := crypto.DecodePublicKey(hf9CompromisedKeyB58)
	if err != nil {
		// Compile-time-known constant; failure here is unreachable.
		return
	}
	hf9Auth := auth.Authority{
		WeightThreshold: 1,
		KeyAuths:        []auth.KeyAuth{{Key: hf9Key, Weight: 1}},
	}

	// Anchor the history row's last_valid_time to the activation block
	// time (matches C++ database_hardfork.cpp:217 which uses
	// `head_block_time()`).
	var lastValidTime uint32
	if dgpo, err := objects.GetDGPO(s); err == nil {
		lastValidTime = dgpo.Time
	}

	for _, name := range hf9CompromisedAccounts {
		acct := auth.AccountName(name)
		ao, err := objects.GetAccountAuthority(s, acct)
		if err != nil {
			continue
		}
		// Archive the old owner authority so recover_account can match
		// the pre-rotation key within HIVE_OWNER_AUTH_RECOVERY_PERIOD.
		objects.PutOwnerAuthorityHistory(s, &objects.OwnerAuthorityHistory{
			Account:                acct,
			PreviousOwnerAuthority: ao.Owner,
			LastValidTime:          lastValidTime,
		})
		// Mirror set_owner() + the bracketed active/posting rewrite
		// from database_hardfork.cpp:223-228.
		ao.Owner = hf9Auth
		ao.PreviousOwnerUpdate = ao.LastOwnerUpdate
		ao.LastOwnerUpdate = lastValidTime
		ao.Active = hf9Auth
		ao.Posting = hf9Auth
		objects.PutAccountAuthority(s, &ao)
	}
}
