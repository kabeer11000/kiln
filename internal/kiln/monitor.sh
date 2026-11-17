# kiln-monitor: runs next to the agent (shared process + network namespace, shared /kiln volume).
# Agent side: append lines to /kiln/outbox.jsonl to report out; read /kiln/inbox.jsonl for messages
# sent with `kiln send`; put files in /kiln/artifacts to have them uploaded at the end.
# Prints one JSON record per line to stdout; kiln reads them through the task's logs.
# Exit 0 = hard deadline reached (kiln reports timed_out). Exit 143 = stopped normally.
set -u
D=/kiln
mkdir -p "$D/artifacts" && touch "$D/inbox.jsonl" "$D/outbox.jsonl" && chmod -R a+rwX "$D"
MPG=$(awk '{l=$0; c=0; for(i=length(l);i>0;i--) if(substr(l,i,1)==")"){c=i;break}; split(substr(l,c+2),f," "); print f[3]}' /proc/$$/stat)
END=$(( $(date +%s) + ${KILN_DEADLINE:-3600} ))
N=0
I=0

snap() {
  T=$(date +%s)
  # Processes outside the monitor's own process group, biggest memory first.
  P=$(awk -v mpg="$MPG" '{
      l=$0; o=index(l,"("); c=0
      for(i=length(l);i>0;i--) if(substr(l,i,1)==")"){c=i;break}
      pid=substr(l,1,o-2); comm=substr(l,o+1,c-o-1); split(substr(l,c+2),f," ")
      if (f[3]==mpg) next
      gsub(/[\\"]/,"",comm)
      printf "%d|{\"pid\":%s,\"ppid\":%s,\"name\":\"%s\",\"state\":\"%s\",\"cpu_ticks\":%.0f,\"rss_kb\":%.0f}\n", f[22]*4, pid, f[2], comm, f[1], f[12]+f[13], f[22]*4
    }' /proc/[0-9]*/stat 2>/dev/null | sort -t'|' -k1 -rn | head -n 80 | cut -d'|' -f2- | paste -sd, -)
  NET=$(awk 'NR>2{ i=index($0,":"); n=substr($0,1,i-1); gsub(/ /,"",n); if(n=="lo") next
      split(substr($0,i+1),v," "); rx+=v[1]; tx+=v[9] } END{printf "{\"rx\":%.0f,\"tx\":%.0f}",rx,tx}' /proc/net/dev 2>/dev/null)
  DISK=$(df -Pk / 2>/dev/null | awk 'NR==2{printf "{\"used_kb\":%s,\"total_kb\":%s}",$3,$2}')
  AF=$(find "$D/artifacts" -type f 2>/dev/null | wc -l)
  AK=$(du -sk "$D/artifacts" 2>/dev/null | cut -f1)
  FILES=""
  if [ "$1" = final ]; then
    FILES=$(find "$D/artifacts" -type f -exec stat -c '%s %n' {} \; 2>/dev/null | head -n 500 | awk '{
        s=$1; $1=""; sub(/^ /,""); sub("^/kiln/artifacts/",""); gsub(/[\\"]/,"")
        printf "%s{\"path\":\"%s\",\"bytes\":%s}", sep, $0, s; sep="," }')
  fi
  echo "{\"kiln\":\"$1\",\"t\":$T,\"procs\":[${P}],\"net\":${NET:-null},\"disk\":${DISK:-null},\"artifacts\":{\"files\":${AF:-0},\"kb\":${AK:-0}},\"files\":[${FILES}]}"
  C=$(awk 'FNR>1 && $4!="0A" && n<200 {printf "%s{\"r\":\"%s\",\"s\":\"%s\"}", sep, $3, $4; sep=","; n++}' /proc/net/tcp /proc/net/tcp6 2>/dev/null)
  echo "{\"kiln\":\"conns\",\"t\":$T,\"conns\":[${C}]}"
  # Per-container memory from ECS; every 6th reading keeps log volume (and its cost) low.
  I=$((I + 1))
  if [ -n "${ECS_CONTAINER_METADATA_URI_V4:-}" ] && { [ $((I % 6)) -eq 1 ] || [ "$1" = final ]; }; then
    S=$(wget -qO- "$ECS_CONTAINER_METADATA_URI_V4/task/stats" 2>/dev/null | tr -d '\n')
    case "$S" in "{"*) echo "{\"kiln\":\"stats\",\"t\":$T,\"stats\":$S}" ;; esac
  fi
}

outbox() {
  L=$(wc -l < "$D/outbox.jsonl")
  if [ "$L" -gt "$N" ]; then
    tail -n +$((N + 1)) "$D/outbox.jsonl" | head -n $((L - N)) | awk -v t="$(date +%s)" '{
        s=substr($0,1,8000); gsub(/\\/,"\\\\",s); gsub(/"/,"\\\"",s); gsub(/\t/," ",s); gsub(/\r/,"",s)
        printf "{\"kiln\":\"outbox\",\"t\":%s,\"line\":\"%s\"}\n", t, s }'
    N=$L
  fi
}

inbox() {
  [ -n "${KILN_INBOX_URL:-}" ] || return 0
  if wget -qO /tmp/inbox.new "$KILN_INBOX_URL" 2>/dev/null && ! cmp -s /tmp/inbox.new "$D/inbox.jsonl"; then
    cp /tmp/inbox.new "$D/.inbox.tmp" && chmod a+r "$D/.inbox.tmp" && mv "$D/.inbox.tmp" "$D/inbox.jsonl"
    echo "{\"kiln\":\"inbox\",\"t\":$(date +%s),\"lines\":$(wc -l < "$D/inbox.jsonl")}"
  fi
}

upload() {
  [ -n "${KILN_UPLOAD_URL:-}" ] || return 0
  # busybox wget can't PUT, so uploads use curl, installed in the background at start.
  # Wait for that install (a second apk would fail on its lock), then retry it once if needed.
  [ -n "${APK:-}" ] && wait "$APK" 2>/dev/null
  command -v curl >/dev/null 2>&1 || apk add --no-cache -q curl >/dev/null 2>&1
  if tar czf /tmp/kiln.tgz -C "$D" artifacts outbox.jsonl inbox.jsonl 2>/dev/null &&
     curl -sf -X PUT -T /tmp/kiln.tgz "$KILN_UPLOAD_URL" >/dev/null; then
    echo "{\"kiln\":\"uploaded\",\"t\":$(date +%s),\"bytes\":$(wc -c < /tmp/kiln.tgz)}"
  else
    echo "{\"kiln\":\"upload_failed\",\"t\":$(date +%s)}"
  fi
}

final() { outbox; snap final; upload; exit "$1"; }
trap 'final 143' TERM INT

APK=""
if [ -n "${KILN_UPLOAD_URL:-}" ]; then apk add --no-cache -q curl >/dev/null 2>&1 & APK=$!; fi
# Inputs: unpack into /kiln/inputs before the agent starts. The agent waits for /kiln/.ready
# (ECS health check + dependsOn HEALTHY). A failed download fails the run: exit 1, never 0.
if [ -n "${KILN_INPUTS_URL:-}" ]; then
  mkdir -p "$D/inputs"
  if wget -qO /tmp/inputs.tgz "$KILN_INPUTS_URL" && tar xzf /tmp/inputs.tgz -C "$D/inputs"; then
    chmod -R a+rwX "$D/inputs"
    echo "{\"kiln\":\"inputs\",\"t\":$(date +%s),\"bytes\":$(wc -c < /tmp/inputs.tgz)}"
    rm -f /tmp/inputs.tgz
  else
    echo "{\"kiln\":\"inputs_failed\",\"t\":$(date +%s)}"
    exit 1
  fi
fi
touch "$D/.ready"
echo "{\"kiln\":\"start\",\"t\":$(date +%s),\"deadline\":$END}"
while [ "$(date +%s)" -lt "$END" ]; do
  snap snapshot; outbox; inbox
  sleep "${KILN_INTERVAL:-5}" & wait $!
done
echo "{\"kiln\":\"deadline\",\"t\":$(date +%s)}"
final 0
