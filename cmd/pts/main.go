package main
import ("fmt";"os";"strconv";"hived-go/internal/blocklog";"hived-go/internal/protocol/block")
func main(){
 r,_:=blocklog.Open(os.Args[1]); defer r.Close()
 start,_:=strconv.Atoi(os.Args[2]); n,_:=strconv.Atoi(os.Args[3])
 var prev uint32
 for b:=uint32(start);b<uint32(start+n);b++{
  raw,err:=r.Block(b); if err!=nil{continue}
  hdr,_,_:=block.UnmarshalSignedHeader(raw)
  gap:=""; if prev!=0 && hdr.Timestamp-prev>3 {gap=fmt.Sprintf("  <-- GAP %ds (%d missed)",hdr.Timestamp-prev,(hdr.Timestamp-prev)/3-1)}
  fmt.Printf("%d ts=%d %s%s\n",b,hdr.Timestamp,hdr.Witness,gap); prev=hdr.Timestamp
 }
}
