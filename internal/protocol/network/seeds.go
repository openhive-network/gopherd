package network

// MainnetSeeds is the default mainnet bootstrap peer list, mirrored from
// hived's libraries/plugins/p2p/include/hive/plugins/p2p/p2p_default_seeds.hpp
// (the !IS_TEST_NET branch). When changing this list, update the C++ source
// in lockstep — drift here means new ghive nodes can't find any peers on
// first start.
var MainnetSeeds = []string{
	"seed.hive.blog:2001",            // blocktrades
	"seed.openhive.network:2001",     // gtg
	"hive-seed.roelandp.nl:2001",     // roelandp
	"hive-seed.arcange.eu:2001",      // arcange
	"node.mahdiyari.info:2001",       // mahdiyari
	"seed.deathwing.me:2001",         // deathwing
	"hive-seed.actifit.io:2001",      // actifit
	"hiveseed.rishipanthee.com:2001", // rishi556
}

// TestnetSeeds mirrors the IS_TEST_NET branch — empty by upstream convention.
// Kept exported for symmetry with MainnetSeeds so callers can pick the right
// list with a build-tagged constant.
var TestnetSeeds = []string{}
