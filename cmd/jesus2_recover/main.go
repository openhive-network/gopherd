package main

import (
	"encoding/hex"
	"fmt"
	"time"
	"unsafe"

	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

func main() {
	expiration, err := time.Parse("2006-01-02T15:04:05", "2016-09-30T23:56:33")
	if err != nil {
		panic(err)
	}

	postingKey, err := crypto.DecodePublicKey("STM7xqXUoCzjB7BXvzE7hCjVcLpgpEVMy3ALy3ygCFX3opkmuhDNd")
	if err != nil {
		panic(err)
	}
	memoKey, err := crypto.DecodePublicKey("STM8Mf3UvSfnoGtBsxi3SjaHYWuUxYgJUJKHWeMsJcLycH6jYMrM5")
	if err != nil {
		panic(err)
	}
	posting := auth.Authority{
		WeightThreshold: 1,
		AccountAuths:    []auth.AccountAuth{{Account: "STM6GVCS7JeftT27", Weight: 1}},
		KeyAuths:        []auth.KeyAuth{{Key: postingKey, Weight: 1}},
	}

	aOp := op.AccountUpdate{
		Account:      "jesus2",
		Posting:      &posting,
		MemoKey:      memoKey,
		JSONMetadata: "",
	}

	wrappedOp := op.OpFromPtr(op.TagAccountUpdate, unsafe.Pointer(&aOp))
	stx := tx.SignedTransaction{
		Transaction: tx.Transaction{
			RefBlockNum:    61473,
			RefBlockPrefix: 1908604542,
			Expiration:     uint32(expiration.Unix()),
			Operations:     []op.Op{wrappedOp},
		},
	}

	buf := tx.AppendTransactionBody(nil, &stx.Transaction)
	fmt.Printf("unsigned bytes (%d): %x\n", len(buf), buf)

	digest := crypto.SHA256Of2(network.OldChainID[:], buf)
	fmt.Printf("digest: %x\n", digest)

	sigHex := "2042af796696cbbf47be4c7f63b45a113bc2584252945d529460ec5f64dd97b4057fc776ba114f26c412d3d0433fc2fa18f1b9fb5c3550e76abd80bb4412e05bcc"
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		panic(err)
	}
	var sig crypto.CompactSignature
	copy(sig[:], sigBytes)
	fmt.Printf("sig canonical: %v\n", sig.IsCanonical())

	recovered, err := crypto.RecoverPublicKey(digest, sig)
	if err != nil {
		fmt.Printf("recover err: %v\n", err)
		return
	}
	fmt.Printf("recovered: %s\n", recovered.EncodeBase58())
	fmt.Println("active:    STM8UsRn6HbmRXkQSTtLRsUmXzb1HEvad5xU6AVY2XTugxYdd8s6P")
	fmt.Println("owner:     STM82yPKqrrrUEhuVYNEHEEqRWU16b4uyRmW6MXPDEzZgpJey9rsB")
	fmt.Println("posting:   STM7xqXUoCzjB7BXvzE7hCjVcLpgpEVMy3ALy3ygCFX3opkmuhDNd")
}
