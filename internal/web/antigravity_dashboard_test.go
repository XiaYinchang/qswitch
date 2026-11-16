package web

import (
	"os/exec"
	"testing"
)

func TestAntigravityDashboardQuotaAndSwitch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js not available for dashboard JavaScript regression")
	}
	script := `
const fs=require('fs'),vm=require('vm');
const source=fs.readFileSync('static/index.html','utf8');
const elements={root:{children:[],innerHTML:'',appendChild(x){this.children.push(x)}},msg:{textContent:'',className:''}};
const requests=[];let prompt='';
const ctx={document:{getElementById:id=>elements[id],createElement:()=>({children:[],appendChild(x){this.children.push(x)}})},confirm:s=>{prompt=s;return true},fetch:async(path,options)=>{requests.push({path,options});return {ok:true,json:async()=>({tools:[]})}}};
vm.createContext(ctx);
vm.runInContext(source.slice(source.indexOf('const $ ='),source.indexOf('$("loginCancel").onclick')),ctx);
const ids=['gemini_5h','gemini_weekly','third_party_5h','third_party_weekly'];
const labels=['Gemini 5 小时','Gemini 周额度','第三方 5 小时','第三方周额度'];
const base={tool:'antigravity',stable_id:'synthetic-account',class:'ok',used_pct:40,buckets:ids.map((id,i)=>({id,used_pct:[100,20,40,30][i]}))};
function render(ac,extra={}){
 elements.root.children=[];
 ctx.render({tools:[{tool:'antigravity',switchable:true,accounts:[ac],...extra}]});
 const section=elements.root.children[0],body=section.children[1];
 return {section,body,card:body.children.find(x=>x.className&&x.className.split(' ').includes('card')).innerHTML};
}
let v=render(base);
if(!v.section.children[0].innerHTML.includes('>Antigravity ')||!v.body.children.some(x=>(x.textContent||'').includes('两组额度都耗尽时才自动切号；单组耗尽时保留当前账号')))throw Error('tool title or strict two-group policy missing');
for(const label of labels)if(!v.card.includes(label))throw Error('missing quota label '+label);
if(!v.card.includes('badge ok')||!v.card.includes('已用 100%')||!v.card.includes('已用 40%'))throw Error('single-group exhaustion was confused with account exhaustion');
for(const missing of [0,1,2,3]){
 const ac={...base,buckets:base.buckets.filter((_,i)=>i!==missing)};
 v=render(ac);
 if(!v.card.includes(labels[missing])||!v.card.includes('用量未知')||v.card.includes('badge ok')||!v.card.includes('额度未知')||!v.card.includes('暂不自动切换'))throw Error('missing window looked usable: '+missing);
}
v=render({...base,class:'unknown',buckets:[]});
if((v.card.match(/用量未知/g)||[]).length!==4||(v.card.match(/class="bar empty"/g)||[]).length!==4||v.card.includes('已用 0%'))throw Error('unknown account invented normal zero-usage bars');
v=render({...base,quota_stale:true,last_probe_class:'unknown',last_quota_at:1790000000,last_probe_at:1790003600});
if(v.card.includes('badge ok')||!v.card.includes('显示历史用量')||!v.card.includes('已用 40%'))throw Error('historical values were presented as confirmed current quota');
if(ctx.usedText(99.99,'antigravity')!=='已用 99%')throw Error('remaining quota rounded to exhausted in presentation');
v=render(base,{capture_error:'synthetic <img src=x onerror=alert(1)> credential backend locked'});
if(!v.section.children[0].innerHTML.includes('收录失败：synthetic &lt;img')||v.section.children[0].innerHTML.includes('<img'))throw Error('capture failure was hidden or not escaped');
const note=ctx.switchNote({tool:'antigravity',busy:true,blocked_by_pid:[123],pending_to:'synthetic-target',accounts:[]});
if(!note.includes('等待桌面 App 和 CLI 会话空闲')||!note.includes('再次核验两组额度')||note.includes('退出会话')||note.includes('重启'))throw Error('incorrect switch wait instructions');
(async()=>{
 await ctx.onClick({target:{closest:()=>({dataset:{act:'switch',tool:'antigravity',id:'synthetic-target'},disabled:false})}});
 if(!prompt.includes('本机 Antigravity 桌面 App 和 CLI')||!prompt.includes('等待会话空闲')||/重启|无缝/.test(prompt))throw Error('switch confirmation promises unsupported behavior');
 const write=requests.find(x=>x.path==='api/switch');
 if(!write||JSON.parse(write.options.body).tool!=='antigravity')throw Error('switch action was not wired');
 if(elements.msg.textContent!=='Antigravity 本机 App 和 CLI 账号已切换。')throw Error('switch feedback claims an App restart');
})().catch(e=>{console.error(e.message);process.exitCode=1});
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("Antigravity dashboard: %v\n%s", err, out)
	}
}
