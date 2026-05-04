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
  endBlock, _ := strconv.Atoi(os.Args[3])
  count := 0
  for n := uint32(1); n <= uint32(endBlock); n++ {
    raw, err := r.Block(n)
    if err != nil { continue }
    _, hdrLen, _ := block.UnmarshalSignedHeader(raw)
    rd := fcraw.NewReader(raw[hdrLen:])
    txCount, _ := rd.ReadVarUint()
    for i := uint32(0); i < txCount; i++ {
      txRes, err := tx.ReadSignedTransaction(rd)
      if err != nil { continue }
      for _, o := range txRes.SignedTx.Operations {
        if fp, ok := o.(op.FeedPublish); ok && string(fp.Publisher) == target {
          count++
          if count <= 5 || count % 50 == 0 {
            fmt.Printf("block %d feed_publish: %s rate=%d/%d\n", n, fp.Publisher, fp.ExchangeRate.Base.Amount, fp.ExchangeRate.Quote.Amount)
          }
        }
      }
    }
  }
  fmt.Printf("Total feed_publish for %s: %d\n", target, count)
}
