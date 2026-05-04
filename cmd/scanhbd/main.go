package main
import (
  "fmt"
  "os"
  "strconv"
  "hived-go/internal/blocklog"
  "hived-go/internal/protocol/op"
  "hived-go/internal/protocol/tx"
  "hived-go/internal/fcraw"
  "hived-go/internal/protocol/block"
)
func main() {
  r, _ := blocklog.Open(os.Args[1])
  defer r.Close()
  target := os.Args[2]
  startBlock, _ := strconv.Atoi(os.Args[3])
  endBlock, _ := strconv.Atoi(os.Args[4])
  for n := uint32(startBlock); n <= uint32(endBlock); n++ {
    raw, err := r.Block(n)
    if err != nil { continue }
    _, hdrLen, _ := block.UnmarshalSignedHeader(raw)
    rd := fcraw.NewReader(raw[hdrLen:])
    txCount, _ := rd.ReadVarUint()
    for i := uint32(0); i < txCount; i++ {
      txRes, err := tx.ReadSignedTransaction(rd)
      if err != nil { continue }
      for _, o := range txRes.SignedTx.Operations {
        switch v := o.(type) {
        case op.Transfer:
          if string(v.From) == target || string(v.To) == target {
            fmt.Printf("block %d Transfer from=%s to=%s amount=%d sym=%x memo=%q\n", n, v.From, v.To, v.Amount.Amount, v.Amount.Symbol, v.Memo)
          }
        case op.TransferToVesting:
          if string(v.From) == target || string(v.To) == target {
            fmt.Printf("block %d TransferToVesting from=%s to=%s amount=%d sym=%x\n", n, v.From, v.To, v.Amount.Amount, v.Amount.Symbol)
          }
        case op.LimitOrderCreate:
          if string(v.Owner) == target {
            fmt.Printf("block %d LimitOrderCreate owner=%s sell=%d sell_sym=%x min_take=%d take_sym=%x\n", n, v.Owner, v.AmountToSell.Amount, v.AmountToSell.Symbol, v.MinToReceive.Amount, v.MinToReceive.Symbol)
          }
        case op.LimitOrderCancel:
          if string(v.Owner) == target {
            fmt.Printf("block %d LimitOrderCancel owner=%s\n", n, v.Owner)
          }
        case op.Convert:
          if string(v.Owner) == target {
            fmt.Printf("block %d Convert owner=%s amount=%d sym=%x\n", n, v.Owner, v.Amount.Amount, v.Amount.Symbol)
          }
        }
      }
    }
  }
}
