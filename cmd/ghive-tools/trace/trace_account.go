package main

import (
	"flag"
	"fmt"
	"os"
	"reflect"

	"hived-go/internal/blocklog"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

func main() {
	path := flag.String("path", "block_log", "block_log path")
	target := flag.String("account", "randaletouri", "account name to trace")
	from := flag.Uint("from", 1, "start block")
	to := flag.Uint("to", 500000, "end block")
	includeProducer := flag.Bool("producer", false, "include blocks produced by target")
	countPow := flag.Bool("count-pow-in-produced", false, "count POW ops in blocks produced by target")
	flag.Parse()
	r, err := blocklog.Open(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer r.Close()
	produced := 0
	totalPowInProduced := 0
	for n := uint32(*from); n <= uint32(*to); n++ {
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		hdr, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			continue
		}
		producedThis := hdr.Witness == *target
		if producedThis && *includeProducer {
			produced++
		}
		rd := fcraw.NewReader(raw[hdrLen:])
		txCount, err := rd.ReadVarUint()
		if err != nil {
			continue
		}
		powCount := 0
		for i := uint32(0); i < txCount; i++ {
			tr, err := tx.ReadSignedTransaction(rd)
			if err != nil {
				break
			}
			for oIdx, o := range tr.SignedTx.Operations {
				if mentions(o, *target) {
					fmt.Printf("block %d tx %d op %d: %T %+v\n", n, i, oIdx, o, o)
				}
				switch o.Operation().(type) {
				case op.Pow, op.Pow2:
					powCount++
				}
			}
		}
		if producedThis && *includeProducer {
			tag := ""
			if *countPow && powCount > 0 {
				tag = fmt.Sprintf(" [contains %d POW]", powCount)
				totalPowInProduced += powCount
			}
			fmt.Printf("block %d PRODUCED_BY %s%s\n", n, *target, tag)
		}
	}
	if *includeProducer {
		fmt.Printf("total %d blocks produced by %s in [%d,%d]; %d POW ops contained in those blocks\n", produced, *target, *from, *to, totalPowInProduced)
	}
}

func mentions(o op.Operation, target string) bool {
	v := reflect.ValueOf(o)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return false
	}
	return scanForString(v, target)
}

func scanForString(v reflect.Value, target string) bool {
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if s == target {
			return true
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if scanForString(v.Field(i), target) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.String {
			for i := 0; i < v.Len(); i++ {
				if v.Index(i).String() == target {
					return true
				}
			}
		}
	case reflect.Interface:
		if !v.IsNil() {
			return scanForString(v.Elem(), target)
		}
	}
	return false
}
