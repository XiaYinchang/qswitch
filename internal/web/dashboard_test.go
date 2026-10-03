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
 if(card.includes('上次已用 0%')||!card.includes('用量未知'))throw Error('unconfirmed account invented zero quota');
}
const old=ctx.bucketHTML({tool:'devin',class:'ok',quota_stale:true,buckets:[{id:'weekly',used_pct:20}]});
if(!old.includes('var(--muted)')||!old.includes('上次已用 20%'))throw Error('historical bucket appears current');
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
