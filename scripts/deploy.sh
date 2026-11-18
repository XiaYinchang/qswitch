#!/usr/bin/env bash
# Install an immutable published release. No compiler runs on deployment hosts.
# Hosts, URL and intended worker scope live in deploy/hosts.tsv.
set -euo pipefail
usage() { echo "usage: $0 --release <tag> [--check|--dry-run] [host ...]"; }
die() { echo "deploy: $*" >&2; exit 1; }
release= mode=apply
targets=()
while [ $# -gt 0 ]; do
 case "$1" in
 --release) [ $# -ge 2 ] || die 'missing release'; release=$2; shift 2 ;;
 --check) mode=check; shift ;;
 --dry-run) mode=dry-run; shift ;;
 -h|--help) usage; exit 0 ;;
 --*) usage >&2; exit 2 ;;
 *) targets+=("$1"); shift ;;
 esac
done
[[ "$release" =~ ^v[0-9][a-zA-Z0-9._-]*$ ]] || die 'an explicit release tag is required'
here=$(cd "$(dirname "$0")/.." && pwd)
manifest="$here/deploy/hosts.tsv"
if [ ${#targets[@]} -eq 0 ]; then
 while read -r name rest; do [[ "$name" == \#* || -z "$name" ]] || targets+=("$name"); done < "$manifest"
fi
for t in "${targets[@]}"; do
 [[ "$t" =~ ^[a-zA-Z0-9_-]+$ ]] || die 'invalid host name'
 [ "$(awk -v h="$t" '$1==h {n++} END {print n+0}' "$manifest")" = 1 ] || die "unknown or duplicate host: $t"
done
tmp=$(mktemp -d "${TMPDIR:-/tmp}/qswitch-release.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
if [ "$mode" != check ]; then
 command -v gh >/dev/null || die 'GitHub CLI is required to download release assets'
 gh release download "$release" --repo XiaYinchang/qswitch --dir "$tmp" --pattern 'SHA256SUMS' --pattern '*.tar.gz'
 python3 - "$tmp" <<'PY'
import hashlib,pathlib,sys
root=pathlib.Path(sys.argv[1]); sums={}
for line in (root/'SHA256SUMS').read_text().splitlines():
 digest,name=line.split(); sums[name.lstrip('*')]=digest
for p in root.glob('*.tar.gz'):
 if hashlib.sha256(p.read_bytes()).hexdigest()!=sums.get(p.name): raise SystemExit('release checksum mismatch: '+p.name)
PY
fi
# Run as the owner of the instance. The Book SSH administrator does not own it.
on() {
 if [ "$remote" = local ]; then (cd "$HOME"; /bin/bash -c "$1")
 elif [ "$account" = - ]; then /usr/bin/ssh -o BatchMode=yes -o ConnectTimeout=10 "$remote" "$1"
 else
  # Manifest values and all callers below use a closed alphabet; no user text.
  /usr/bin/ssh -o BatchMode=yes -o ConnectTimeout=10 "$remote" "cd /Users/$account && sudo -u $account -H /bin/bash -c '$1'"
 fi
}
failed=0
for t in "${targets[@]}"; do
 read -r name remote os arch account public_url scope < <(awk -v h="$t" '$1==h {print}' "$manifest")
 [[ "$remote" =~ ^[a-zA-Z0-9._-]+$ && "$account" =~ ^[a-zA-Z0-9_-]+$ ]] || die 'invalid host manifest'
 [ "$os" = linux ] || [ "$os" = darwin ] || die 'unsupported service host'
 echo "$t: $mode $release ($os/$arch, scope=$scope)"
 if [ "$mode" != check ]; then
  stage="$tmp/$os-$arch"
  if [ ! -d "$stage" ]; then mkdir "$stage"; tar -xzf "$tmp/qswitch-$os-$arch.tar.gz" -C "$stage"; fi
  for bin in qswitch qswitchd; do [ -f "$stage/$bin" ] || die "release is missing $bin"; done
 fi
 # Check is read-only. A dry run verifies the release and live preconditions,
 # but does not upload files, restart the daemon, or touch account data.
 if [ "$mode" != apply ]; then
  on "python3 - $release $mode $scope" <<'PY' || failed=1
import json,pathlib,sqlite3,subprocess,sys,urllib.request
want,mode,scope=sys.argv[1:]; home=pathlib.Path.home(); data=home/'.qswitch'
health=json.load(urllib.request.urlopen('http://127.0.0.1:7432/api/health',timeout=5))
cli=subprocess.check_output([str(home/'.local/bin/qswitch'),'version'],text=True).strip().splitlines()[0]
db=sqlite3.connect('file:'+str(data/'state.db')+'?mode=ro',uri=True)
pending=db.execute('select count(*) from pending_switch').fetchone()[0];db.close()
assert not pending and not (data/'managed-claude/pending.json').exists(), 'a switch is in progress'
info=health.get('instance',{})
print(json.dumps({'daemon':health['version'],'cli':cli,'instance':info.get('id'),'scope':info.get('managed_tools'),'target':want,'public_data_checked':False}))
if mode=='check':
 assert health['version']==want and cli==want,'version drift'
 expected=['codex','grok','cursor','devin','zcode','kimi','claude','antigravity'] if scope=='all' else scope.split(',')
 assert sorted(info.get('managed_tools',[]))==sorted(expected),'worker scope drift'
PY
  continue
 fi
 on "umask 077; mkdir .qswitch/deploy-$release"
 for bin in qswitch qswitchd; do
  on "cat > .qswitch/deploy-$release/$bin" < "$stage/$bin"
 done
 # Pass expected binary hashes as arguments, never trust only an archive name.
 h1=$(shasum -a 256 "$stage/qswitch" | awk '{print $1}')
 h2=$(shasum -a 256 "$stage/qswitchd" | awk '{print $1}')
 if ! on "python3 - $release $scope $h1 $h2" <<'PY'
import hashlib,json,os,pathlib,shutil,sqlite3,subprocess,sys,time,urllib.request
want,scope,h1,h2=sys.argv[1:]; home=pathlib.Path.home(); data=home/'.qswitch'; bindir=home/'.local/bin'; stage=data/('deploy-'+want)
mac=sys.platform=='darwin'; uid=os.getuid(); plist=home/'Library/LaunchAgents/com.qswitch.plist'
def run(args): return subprocess.run(args,check=True,capture_output=True,text=True).stdout.strip()
def health(): return json.load(urllib.request.urlopen('http://127.0.0.1:7432/api/health',timeout=4))
def safe():
 # Closing the daemon can remove WAL/SHM files. SQLite on macOS cannot
 # always reopen that WAL database with mode=ro. Open only an existing
 # database so SQLite can recreate its sidecars, then prohibit SQL writes.
 db=sqlite3.connect('file:'+str(data/'state.db')+'?mode=rw',uri=True)
 try:
  db.execute('pragma query_only=ON')
  n=db.execute('select count(*) from pending_switch').fetchone()[0]
 finally: db.close()
 assert n==0 and not (data/'managed-claude/pending.json').exists(),'switch in progress; deployment refused'
def stop():
 if mac:
  subprocess.run(['launchctl','bootout',f'gui/{uid}/com.qswitch'],capture_output=True,text=True)
  # launchctl bootout can return before launchd finishes removing the job; wait
  # until the label disappears so bootstrap and print do not race teardown.
  for _ in range(50):
   present=subprocess.run(['launchctl','print',f'gui/{uid}/com.qswitch'],capture_output=True,text=True)
   if present.returncode!=0: return
   time.sleep(0.1)
  raise RuntimeError('unable to stop launchd service')
 else: run(['systemctl','--user','stop','qswitchd.service'])
def start():
 if not mac: return run(['systemctl','--user','start','qswitchd.service'])
 result=subprocess.run(['launchctl','bootstrap',f'gui/{uid}',str(plist)],capture_output=True,text=True)
 if result.returncode==0: return
 # An SSH login may lack the owner's GUI session. Do not bootstrap twice
 # if launchd registered the job despite reporting an error to this session.
 present=subprocess.run(['launchctl','print',f'gui/{uid}/com.qswitch'],capture_output=True,text=True)
 if present.returncode==0: return
 try: run(['sudo','-n','launchctl','asuser',str(uid),'launchctl','bootstrap',f'gui/{uid}',str(plist)])
 except subprocess.CalledProcessError as err:
  raise RuntimeError(f'launchd bootstrap failed in current and GUI sessions (exit {result.returncode}, {err.returncode})') from err
def wait(version):
 for _ in range(60):
  try:
   got=health()
   if got['version']==version: return got
  except Exception: pass
  time.sleep(1)
 raise RuntimeError('daemon did not reach '+version)
try:
 old=health(); safe()
 for name,digest in [('qswitch',h1),('qswitchd',h2)]:
  p=stage/name
  assert hashlib.sha256(p.read_bytes()).hexdigest()==digest,'staged checksum mismatch'
  p.chmod(0o755)
  arg='version' if name=='qswitch' else '--version'
  assert run([str(p),arg]).splitlines()[0]==want,'staged version mismatch'
  shutil.copy2(bindir/name,stage/(name+'.previous'))
except Exception as err:
 shutil.rmtree(stage)
 raise SystemExit("preflight failed; running programs unchanged: "+str(err))
# No new switch can begin after the daemon has stopped. Re-check durable work
# before replacing either executable, and resume the old daemon on rejection.
stopped=False; swapped=False
try:
 stop(); stopped=True; safe()
 swapped=True # Any partial replacement must restore both previous binaries.
 for name in ['qswitch','qswitchd']: os.replace(stage/name,bindir/name)
 start(); stopped=False
 got=wait(want)
 assert got.get('instance',{}).get('id'),'missing instance identity'
 assert not old.get('instance',{}).get('id') or old['instance']['id']==got['instance']['id'],'instance identity changed'
 tools=got['instance']['managed_tools']
 expected=['codex','grok','cursor','devin','zcode','kimi','claude','antigravity'] if scope=='all' else scope.split(',')
 assert sorted(tools)==sorted(expected),'worker scope changed'
 assert run([str(bindir/'qswitch'),'version']).splitlines()[0]==want,'CLI did not update'
 overview=json.load(urllib.request.urlopen('http://127.0.0.1:7432/api/overview',timeout=15))
 visible=[]
 for item in overview['tools']:
  if item['tool']=='grokbot' and item.get('derived_from')=='cursor' and 'cursor' in tools: continue
  assert not item.get('derived_from'),'unexpected derived tool'
  visible.append(item['tool'])
 assert sorted(visible)==sorted(tools),'overview scope mismatch'
 time.sleep(10)
 assert health()['version']==want,'daemon did not stay on target version'
 receipt={'release':want,'previous':old['version'],'instance':got['instance'],'checked_at':int(time.time()),'sha256':{'qswitch':h1,'qswitchd':h2}}
 out=data/'deployment.json.next'; out.write_text(json.dumps(receipt,indent=2)+'\n');out.chmod(0o600);os.replace(out,data/'deployment.json')
 shutil.rmtree(stage)
 print(json.dumps({'result':'deployed','release':want,'instance':got['instance'],'rollback_files_removed':True}))
except Exception as err:
 try:
  if swapped:
   stop()
   for name in ['qswitch','qswitchd']: os.replace(stage/(name+'.previous'),bindir/name)
   start();stopped=False;wait(old['version'])
  elif stopped: start();stopped=False;wait(old['version'])
  shutil.rmtree(stage)
  print('deployment failed; previous version restored',file=sys.stderr)
 except Exception as recovery:
  print('rollback needs attention; recovery files retained at '+str(stage)+': '+str(recovery),file=sys.stderr)
 raise SystemExit(str(err))
PY
 then failed=1; fi
done
exit "$failed"
