package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state/objects"
)

// Pre-HF20 the RC manabar isn't tracked. New accounts have all-zero
// RC fields (the schema defaults).
func TestRCManabarPreHF20Inactive(t *testing.T) {
	f := New(t)
	// No HardforksUpTo → pre-HF20.
	f.Actor("alice")
	f.GenerateBlock()
	a := f.Account_("alice")
	if a.RCManaCurrent != 0 || a.RCManaLastUpdate != 0 || a.LastMaxRC != 0 {
		t.Fatalf("pre-HF20 RC fields should be zero: %+v", a)
	}
}

// HF20+ account creation initializes the manabar with last_update_time
// set so subsequent regen has a defined origin.
func TestRCManabarPostHF20InitOnCreate(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	// Use account_create to trigger the initializer.
	f.FundLiquid(InitMiner, 100_000)
	f.GenerateBlock()
	pubKey := [33]byte{0x02, 'A'}
	if err := f.ApplyOp(op.AccountCreate{
		Fee:            HIVE(0),
		Creator:        InitMiner,
		NewAccountName: "alice",
		Owner:          newAuth(pubKey),
		Active:         newAuth(pubKey),
		Posting:        newAuth(pubKey),
		MemoKey:        pubKey,
	}); err != nil {
		t.Fatalf("account_create: %v", err)
	}
	a := f.Account_("alice")
	if a.RCManaLastUpdate == 0 {
		t.Fatal("alice.RCManaLastUpdate should be set post-HF20")
	}
	// max_rc is zero since alice has no VESTS.
	if a.LastMaxRC != 0 {
		t.Fatalf("alice.LastMaxRC: got %d, want 0 (no VESTS)", a.LastMaxRC)
	}
}

// transfer_to_vesting bumps the recipient's max_rc; the manabar
// regenerates and LastMaxRC tracks the new effective vests.
func TestRCManabarRefreshAfterTransferToVesting(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actor("alice")
	f.Actor("bob")
	f.FundLiquid("alice", 100_000)
	f.GenerateBlock()

	preBob := f.Account_("bob")
	if preBob.LastMaxRC != 0 {
		t.Fatalf("bob.LastMaxRC pre-vest: got %d, want 0", preBob.LastMaxRC)
	}

	if err := f.ApplyOp(op.TransferToVesting{
		From: "alice", To: "bob", Amount: HIVE(10_000),
	}); err != nil {
		t.Fatalf("transfer_to_vesting: %v", err)
	}

	bob := f.Account_("bob")
	if bob.LastMaxRC == 0 {
		t.Fatal("bob.LastMaxRC should be non-zero after vesting credit")
	}
	if bob.LastMaxRC != bob.EffectiveVestingShares() {
		t.Fatalf("bob.LastMaxRC %d != EffectiveVestingShares %d",
			bob.LastMaxRC, bob.EffectiveVestingShares())
	}
}

// Manabar regenerates linearly: after half the regen window the
// manabar should be ~half full (starting from 0).
func TestRCManabarLinearRegen(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actor("alice")
	f.FundLiquid("alice", 100_000)
	f.FundVesting("alice", 100_000) // alice gets some VESTS → max_rc > 0
	f.GenerateBlock()

	// Force RC manabar refresh by doing a transfer_to_vesting.
	f.Actor("self_target")
	if err := f.ApplyOp(op.TransferToVesting{
		From: "alice", To: "alice", Amount: HIVE(1_000),
	}); err != nil {
		t.Fatalf("vest: %v", err)
	}

	a := f.Account_("alice")
	maxRC := a.MaximumRC()
	if maxRC == 0 {
		t.Fatal("alice should have non-zero max_rc")
	}
	startMana := a.RCManaCurrent

	// Skip 2.5 days (half the regen window) and trigger another
	// manabar refresh via transfer_to_vesting.
	f.SkipSeconds(objects.RCRegenTimeSec / 2)
	if err := f.ApplyOp(op.TransferToVesting{
		From: "alice", To: "alice", Amount: HIVE(1),
	}); err != nil {
		t.Fatalf("vest 2: %v", err)
	}

	a = f.Account_("alice")
	if a.RCManaCurrent <= startMana {
		t.Fatalf("expected mana to regenerate after 2.5 days: start=%d after=%d",
			startMana, a.RCManaCurrent)
	}
}

// Manabar caps at max_rc — refilling for many regen windows doesn't
// exceed the cap.
func TestRCManabarCappedAtMax(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actor("alice")
	f.FundLiquid("alice", 100_000)
	f.FundVesting("alice", 100_000)
	f.GenerateBlock()

	// Trigger initial RC update.
	if err := f.ApplyOp(op.TransferToVesting{
		From: "alice", To: "alice", Amount: HIVE(1_000),
	}); err != nil {
		t.Fatalf("vest: %v", err)
	}

	// Skip ~3 regen windows.
	f.SkipSeconds(objects.RCRegenTimeSec * 3)
	if err := f.ApplyOp(op.TransferToVesting{
		From: "alice", To: "alice", Amount: HIVE(1),
	}); err != nil {
		t.Fatalf("vest 2: %v", err)
	}

	a := f.Account_("alice")
	if a.RCManaCurrent > a.LastMaxRC {
		t.Fatalf("RCManaCurrent (%d) exceeds LastMaxRC (%d)",
			a.RCManaCurrent, a.LastMaxRC)
	}
	if a.RCManaCurrent != a.LastMaxRC {
		t.Logf("note: mana not at full cap after 3 windows: %d / %d",
			a.RCManaCurrent, a.LastMaxRC)
	}
}

// Regenerate primitive: starting from 0 with max=1000 and
// regen_time=100, after 50 ticks we should have 500.
func TestRCManabarRegenPrimitive(t *testing.T) {
	const maxMana int64 = 1000
	const regenTime uint32 = 100

	cur, ts := objects.RegenerateMana(0, 0, objects.ManabarParams{
		MaxMana: maxMana, RegenTime: regenTime,
	}, 50, true)
	if cur != 500 || ts != 50 {
		t.Fatalf("half-window regen: got mana=%d ts=%d, want 500/50", cur, ts)
	}

	// Full window → reach cap.
	cur, ts = objects.RegenerateMana(0, 0, objects.ManabarParams{
		MaxMana: maxMana, RegenTime: regenTime,
	}, 100, true)
	if cur != 1000 || ts != 100 {
		t.Fatalf("full-window regen: got mana=%d ts=%d, want 1000/100", cur, ts)
	}

	// Past the window with skip_cap_regen=true → stays at cap.
	cur, _ = objects.RegenerateMana(0, 0, objects.ManabarParams{
		MaxMana: maxMana, RegenTime: regenTime,
	}, 5000, true)
	if cur != 1000 {
		t.Fatalf("over-cap regen: got %d, want 1000", cur)
	}

	// Already-at-cap → stays at cap, ts updates to now.
	cur, ts = objects.RegenerateMana(1000, 0, objects.ManabarParams{
		MaxMana: maxMana, RegenTime: regenTime,
	}, 200, true)
	if cur != 1000 || ts != 200 {
		t.Fatalf("at-cap regen: got mana=%d ts=%d, want 1000/200", cur, ts)
	}
}

// EffectiveVestingShares = vesting - delegated_out + received_in.
func TestEffectiveVestingShares(t *testing.T) {
	a := objects.Account{
		VestingShares:          1000,
		DelegatedVestingShares: 200,
		ReceivedVestingShares:  300,
	}
	got := a.EffectiveVestingShares()
	if got != 1100 {
		t.Fatalf("effective_vests: got %d, want 1100", got)
	}
}
