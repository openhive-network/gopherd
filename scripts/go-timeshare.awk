# Pass 1: count witness appearance freq across all FEED-SCHED lines.
# Pass 2: for each hour, the timeshare = schedule member with min freq.
# Usage: awk -f go-timeshare.awk onset.err   (run twice via getline trick)
/FEED-SCHED/ {
  blk=""; sched="";
  for(i=1;i<=NF;i++){ if($i ~ /^block=/){blk=substr($i,7)} }
  match($0,/schedule=\[[^]]*\]/); s=substr($0,RSTART+10,RLENGTH-11); gsub(/,/," ",s);
  lines[++nl]=blk"|"s;
  split(s,a," "); for(k in a) freq[a[k]]++;
}
END{
  for(i=1;i<=nl;i++){
    split(lines[i],p,"|"); blk=p[1]; split(p[2],a," ");
    minf=1e9; ts="";
    for(k=1;k<=length(a);k++){ if(freq[a[k]]<minf){minf=freq[a[k]]; ts=a[k]} }
    print blk, ts, "(freq="minf")";
  }
}
