package web

import (
	"os/exec"
	"testing"
)

func TestDashboardHistoricalQuotaAndProbeFeedback(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js not available for dashboard JavaScript regression")
	}
	// Evaluate the shipped JavaScript against a small DOM/fetch fixture, without
	// starting a browser or issuing any network requests.
	script := `
const fs=require('fs'),vm=require('vm');
const source=fs.readFileSync('static/index.html','utf8');
const elements={root:{children:[],innerHTML:'',appendChild(x){this.children.push(x)}},msg:{textContent:'',className:''}};
let result={class:'unknown',updated:false,source:'http'};
const ctx={document:{getElementById:id=>elements[id],createElement:()=>({children:[],appendChild(x){this.children.push(x)}})},fetch:async path=>({ok:true,json:async()=>path==='/api/probe'?result:{tools:[]}})};
vm.createContext(ctx);
vm.runInContext(source.slice(source.indexOf('const $ ='),source.indexOf('$("loginCancel").onclick')),ctx);
for(const cls of ['unknown','expired']){
 elements.root.children=[];
 ctx.render({tools:[{tool:'devin',accounts:[{tool:'devin',stable_id:'never',class:cls,quota_stale:true,used_pct:0}]}]});
 const card=elements.root.children[0].children[0].innerHTML;
 if(card.includes('已用 0%')||!card.includes('用量未知'))throw Error('unconfirmed account invented zero quota');
}
const old=ctx.bucketHTML({tool:'devin',class:'ok',quota_stale:true,buckets:[{id:'weekly',used_pct:20}]});
if(!old.includes(ctx.usedColor(20))||!old.includes('已用 20%')||old.includes('上次已用'))throw Error('historical bucket uses different usage semantics');
const unknownBucket=ctx.bucketHTML({tool:'kimi',class:'unknown',buckets:[{id:'5h'},{id:'weekly',used_pct:null}]});
if(unknownBucket.includes('已用 0%')||unknownBucket.includes('width:0%'))throw Error('missing bucket usage invented zero quota');
for(const stale of [false,true]){
 elements.root.children=[];
 const ac={tool:'cursor',stable_id:'fixture',class:'soft',used_pct:100,quota_stale:stale,last_probe_class:stale?'unknown':'soft',last_probe_at:1790003600,last_quota_at:stale?1790000000:1790003600,buckets:[{id:'auto',used_pct:1},{id:'api',used_pct:100}]};
 ctx.render({tools:[{tool:'cursor',accounts:[ac]}]});
 const card=elements.root.children[0].children[0].innerHTML;
 if(!card.includes('width:100%;background:'+ctx.usedColor(100))||!card.includes('已用 100%')||card.includes('上次已用'))throw Error('100 percent usage differs by observation status');
 if(!card.includes('用量记录 '+ctx.fmtTime(ac.last_quota_at)))throw Error('confirmed record time missing');
 if(stale&&(!card.includes('显示历史用量')||!card.includes('最近查询 '+ctx.fmtTime(ac.last_probe_at))||card.includes('badge soft')))throw Error('historical status was not separated from usage');
}
elements.root.children=[];
ctx.render({tools:[{tool:'devin',accounts:[{tool:'devin',stable_id:'fixture',class:'exhausted',used_pct:100,last_quota_at:1790000000}]}]});
const exhausted=elements.root.children[0].children[0].innerHTML;
if(!exhausted.includes('width:100%;background:'+ctx.usedColor(100))||!exhausted.includes('已用 100%')||!exhausted.includes('用量记录'))throw Error('aggregate exhaustion differs from bucket exhaustion');
(async()=>{
 for(const cls of ['unknown','expired','ok']){
  result={class:cls,updated:cls==='ok'};
  const button={dataset:{act:'probe',tool:'devin',id:'fixture'},disabled:false};
  await ctx.onClick({target:{closest:()=>button}});
  if(cls==='ok'){
   if(elements.msg.textContent!=='用量已更新')throw Error('success feedback missing');
  }else if(elements.msg.textContent==='用量已更新'||elements.msg.className!=='msg err')throw Error('failed probe claimed success');
 }
})().catch(error=>{console.error(error.message);process.exitCode=1});
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("dashboard regression: %v\n%s", err, out)
	}
}
