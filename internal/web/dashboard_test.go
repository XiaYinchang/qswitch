package web

import (
	"encoding/json"
	"os/exec"
	"testing"
)

// The production JavaScript is evaluated directly. Network and DOM boundaries
// are synthetic; no provider credentials or running service are involved.
func dashboardScript(t *testing.T, checks string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js not available for dashboard regression")
	}
	checkJSON, err := json.Marshal(checks)
	if err != nil {
		t.Fatal(err)
	}
	script := `
const fs=require('fs'),vm=require('vm'),assert=require('assert/strict');
const source=fs.readFileSync('static/index.html','utf8');
const elements=new Map();
function element(){return {textContent:'',className:'',hidden:false,disabled:false,open:false,dataset:{},children:[],innerHTML:'',contains(){return false},querySelectorAll(){return []},setAttribute(){},removeAttribute(){},appendChild(n){this.children.push(n)},focus(){this.focused=true},showModal(){this.open=true},close(){this.open=false}}}
let fetchImpl=async()=>({ok:true,status:200,json:async()=>({instance:{id:'fixture-a',name:'Fixture A'},tools:[]})});
const ctx={assert,URL,Intl,AbortController,DOMException,TypeError,console,setTimeout,clearTimeout,Date,Map,Set,Promise,location:{href:'http://localhost:7432/',hash:'',assign(){}},navigator:{clipboard:{writeText:async()=>{}}},document:{hidden:false,getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id)},querySelector(){return element()},createElement:element,activeElement:null},fetch:(...args)=>fetchImpl(...args),setFetch(fn){fetchImpl=fn},elements};
vm.createContext(ctx);
vm.runInContext(source.slice(source.indexOf('// qswitch-ui-start'),source.indexOf('// qswitch-ui-bootstrap'))+` + string(checkJSON) + `,ctx);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("dashboard regression: %v\n%s", err, out)
	}
}

func TestDashboardHistoricalQuotaAndFeedback(t *testing.T) {
	dashboardScript(t, `
const tool={tool:'devin',capabilities:{probe:{available:true},switch:{available:true}}};
for(const cls of ['unknown','expired']){
 const html=accountHTML({tool:'devin',stable_id:'fixture',class:cls,used_pct:0},tool);
 assert(!html.includes('已用 0%'));assert(html.includes('用量未知'));
}
const old={tool:'cursor',stable_id:'fixture',class:'soft',quota_stale:true,used_pct:100,last_probe_class:'unknown',last_probe_at:1790003600,last_quota_at:1790000000,buckets:[{id:'auto',used_pct:1},{id:'api',used_pct:100}]};
assert(bucketHTML(old).includes('已用 100%'));assert(quotaNote(old).includes('显示历史用量'));assert(quotaNote(old).includes('用量记录'));assert(!accountStatus(old).includes('接近用尽'));
assert(bucketHTML({tool:'kimi',buckets:[{id:'5h'},{id:'weekly',used_pct:null}]}).includes('用量未知'));
assert(!bucketHTML({tool:'kimi',buckets:[{id:'5h'}]}).includes('已用 0%'));
const html=accountHTML({tool:'devin',stable_id:'synthetic',email:'<img onerror=bad>',class:'unknown'},tool);
assert(html.includes('&lt;img'));assert(!html.includes('<img onerror'));
assert(environmentURL({id:'env-b',name:'B',url:'https://example.test/qsw/'}).includes('expected_instance=env-b'));
assert.equal(environmentURL({id:'env-b',url:'javascript:alert(1)'}),'');
assert.equal(environmentURL({id:'env-b',url:'https://user:secret@example.test/'}),'');
`)
}

func TestDashboardInstanceGuardsAndReadOrdering(t *testing.T) {
	dashboardScript(t, `
render=()=>{};renderMessage=()=>{};
(async()=>{
 state.instance={id:'fixture-a',name:'A'};state.trustedID='fixture-a';state.online=true;
 let calls=[];setFetch(async(path,opts)=>{calls.push({path,opts});return {ok:true,status:200,json:async()=>({})}});
 await api('api/capture',{method:'POST',body:{tool:'codex'}});
 assert.equal(calls[0].opts.headers['X-Qswitch-Instance'],'fixture-a');
 state.expected='fixture-b';setFetch(async()=>({ok:true,status:200,json:async()=>({instance:{id:'fixture-a',name:'A'}})}));
 assert.equal(await connect(),false);assert(state.fatal.includes('不匹配'));
 let wrote=false;setFetch(async()=>{wrote=true;return {ok:true,status:200,json:async()=>({})}});
 await assert.rejects(api('api/auto',{method:'POST',body:{tool:'codex',enabled:true}}));assert.equal(wrote,false);
 state.fatal='';state.expected='';state.online=true;state.messages.set('codex',{text:'操作失败，须处理',error:true});
 let first,second,count=0;setFetch(()=>new Promise(resolve=>{if(++count===1)first=resolve;else second=resolve}));
 const a=loadSnapshot(),b=loadSnapshot();
 second({ok:true,status:200,json:async()=>({instance:{id:'fixture-a',name:'newer'},tools:[]})});await b;
 first({ok:true,status:200,json:async()=>({instance:{id:'fixture-a',name:'older'},tools:[]})});await a;
 assert.equal(state.instance.name,'newer');assert.equal(state.messages.get('codex').text,'操作失败，须处理');
 setFetch(async()=>({ok:true,status:200,json:async()=>({instance:{id:'wrong'},tools:[]})}));
 await loadSnapshot();assert(state.fatal.includes('其他实例'));assert.equal(canWrite(),false);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardMutationsDoNotDuplicateOrRetryUnknownResults(t *testing.T) {
	dashboardScript(t, `
render=()=>{};renderMessage=()=>{};
(async()=>{
 state.instance={id:'fixture-a',name:'A'};state.trustedID='fixture-a';state.online=true;
 let postCount=0,finish;
 setFetch((path,opts)=>opts.method==='POST'?(postCount++,new Promise(resolve=>finish=resolve)):Promise.resolve({ok:true,status:200,json:async()=>({instance:{id:'fixture-a',name:'A'},tools:[]})}));
 const first=mutate('codex','收录','api/capture',{tool:'codex'});
 assert.equal(await mutate('codex','收录','api/capture',{tool:'codex'}),false);assert.equal(postCount,1);
 finish({ok:true,status:200,json:async()=>({captured:[]})});await first;assert.equal(state.operations.size,0);
 let timedWrites=0;setFetch((path,opts)=>new Promise((resolve,reject)=>{timedWrites++;opts.signal.addEventListener('abort',()=>reject(new DOMException('aborted','AbortError')),{once:true})}));
 try{await api('api/switch',{method:'POST',body:{tool:'codex'},timeout:2});throw Error('timeout was not reported')}catch(e){assert.equal(e.unknown,true);assert(e.message.includes('结果尚未确认'))}
 assert.equal(timedWrites,1);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardFocusDisclosureAndLoginPolling(t *testing.T) {
	dashboardScript(t, `
const old={dataset:{focus:'same-button'}},replacement={dataset:{focus:'same-button'},disabled:false,focus(){this.focused=true}},disclosure={dataset:{detail:'account-details'},open:false};
const node={contains:n=>n===old,querySelectorAll:selector=>selector==='details[open][data-detail]'?[{dataset:{detail:'account-details'}}]:selector==='details[data-detail]'?[disclosure]:[replacement]};
document.activeElement=old;patchHTML(node,'changed');assert.equal(disclosure.open,true);assert.equal(replacement.focused,true);
render=()=>{};
(async()=>{
 state.instance={id:'fixture-a'};state.trustedID='fixture-a';state.online=true;
 ui.login={generation:2,id:'synthetic-login',tool:'codex',expires:Date.now()+60000,polling:false,timer:null};
 let count=0,finish;setFetch(()=>{count++;return new Promise(resolve=>finish=resolve)});
 const first=pollLogin(2);await pollLogin(2);assert.equal(count,1);
 finish({ok:true,status:200,json:async()=>({status:'error',error:'Synthetic denial'})});await first;
 assert.equal($('loginRetry').hidden,false);assert.equal(ui.login.polling,false);
 ui.login.expires=Date.now()-1;await pollLogin(2);assert.equal(count,1);assert.equal($('loginExpiry').textContent,'已过期');
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardDerivedToolDoesNotExposeAccountMutations(t *testing.T) {
	dashboardScript(t, `
const tool={tool:'grokbot',derived_from:'cursor',capabilities:{probe:{available:true},switch:{available:false,reason:'由Cursor账号派生的只读用量'}}};
const html=accountHTML({tool:'grokbot',stable_id:'synthetic',email:'fixture@example.test',class:'unknown'},tool);
assert(html.includes('data-act="probe"'));assert(!html.includes('data-act="forget"'));assert(!html.includes('data-act="switch"'));
renderToolSettings(tool);assert($('toolSettings').innerHTML.includes('管理 Cursor 账号'));assert(!$('toolSettings').innerHTML.includes('data-act="auto"'));assert.equal(actionTool('grokbot'),'cursor');
const native={tool:'antigravity',auto_reason:'自动切换已关闭',capabilities:{login:{available:false,reason:'请在官方客户端登录',command:'agy'}}};
assert(!loginGuidance(native).includes('自动切换已关闭'));assert(loginGuidance(native).includes('请在官方客户端登录'));
`)
}

func TestDashboardAutoConfigurationAndEffectiveState(t *testing.T) {
	dashboardScript(t, `
(async()=>{
 state.instance={id:'fixture-a'};state.trustedID='fixture-a';state.online=true;
 const tool={tool:'antigravity',enabled:true,auto:false,auto_reason:'全局自动切换已关闭；仅开启此工具不会开启全局开关',capabilities:{auto_switch:{available:true}}};
 state.overview={tools:[tool]};renderToolSettings(tool);
 assert($('toolSettings').innerHTML.includes('aria-checked="true"'));assert($('toolSettings').innerHTML.includes('已启用'));
 let sent,feedback;mutate=async(t,label,path,body,done)=>{sent=body;feedback=done({tools:[{tool:t,enabled:false,auto:false}]})};
 const button={disabled:false,dataset:{act:'auto',tool:'antigravity'}};
 await onClick({target:{closest:s=>s==='[data-act]'?button:null}});
 assert.equal(sent.enabled,false);assert(feedback.includes('已停用'));
 tool.enabled=false;renderToolSettings(tool);assert($('toolSettings').innerHTML.includes('aria-checked="false"'));
 mutate=async(t,label,path,body,done)=>{sent=body;feedback=done({tools:[{...tool,enabled:true}]})};
 await onClick({target:{closest:s=>s==='[data-act]'?button:null}});
 assert.equal(sent.enabled,true);assert(feedback.includes('已启用，尚未生效'));assert(feedback.includes('全局自动切换已关闭'));assert(!feedback.includes('已启用并生效'));
 assert(autoResult({tools:[{...tool,enabled:true,auto:true}]},tool.tool).includes('已启用并生效'));
 assert(autoResult({tools:[]},tool.tool).includes('请刷新状态核对'));
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardSwitchGuidanceMatchesClientBoundaries(t *testing.T) {
	dashboardScript(t, `
assert(switchImpact('codex').includes('需先退出'));assert(switchImpact('codex').includes('只更新 Codex CLI'));assert(switchImpact('codex').includes('不恢复桌面登录'));
assert(switchImpact('grok').includes('只更新 Grok CLI'));assert(switchImpact('grok').includes('不修改 Grok Bot 桌面登录'));
state.overview={tools:[{tool:'cursor',capabilities:{desktop_sync:{available:true}}}]};
assert(switchImpact('cursor').includes('请先退出 Cursor 桌面端'));assert(switchImpact('cursor').includes('同步更新 IDE 和 CLI'));assert(switchImpact('cursor').includes('桌面端需自行重新打开'));
for(const tool of ['codex','grok','cursor'])assert(!switchImpact(tool).includes('关闭并重启'));
assert(switchNote({tool:'codex',busy:true}).includes('需先退出'));assert(switchNote({tool:'cursor',busy:true,capabilities:{desktop_sync:{available:true}}}).includes('请先退出'));
state.overview.tools[0].capabilities.desktop_sync.available=false;
assert(switchImpact('cursor').includes('只更新 Cursor CLI'));assert(switchImpact('cursor').includes('不管理 Cursor IDE'));assert(!switchImpact('cursor').includes('请先退出'));
assert(switchNote({tool:'cursor',busy:true}).includes('不管理 Cursor IDE'));assert(!switchNote({tool:'cursor',busy:true}).includes('请先退出'));
`)
}

func TestDashboardUnavailableAutoCanStillBeDisabled(t *testing.T) {
	dashboardScript(t, `
state.instance={id:'fixture-a'};state.trustedID='fixture-a';state.online=true;
const tool={tool:'cursor',enabled:true,auto:false,capabilities:{auto_switch:{available:false,reason:'当前平台不可用'}}};
const toggle=()=>{renderToolSettings(tool);return $('toolSettings').innerHTML.match(/<button[^>]+data-act="auto"[^>]*>/)[0]};
assert(!toggle().includes('disabled'));tool.enabled=false;assert(toggle().includes('disabled'));
tool.enabled=true;state.online=false;assert(toggle().includes('disabled'));state.online=true;state.operations.set('cursor',{label:'查询'});assert(toggle().includes('disabled'));
`)
}

func TestDashboardBotProbeFeedbackUsesIndependentResult(t *testing.T) {
	dashboardScript(t, `
(async()=>{
 const bot=(cls)=>({class:'ok',updated:true,buckets:[{id:'api',class:'ok'},{id:'bot',class:cls}]});
 for(const cls of ['unknown','expired','']){const text=probeFeedback(bot(cls),'grokbot','account');assert(text.includes('未能确认')||text.includes('未确认'));assert(!text.includes('查询完成'))}
 assert(probeFeedback({class:'ok',updated:true},'grokbot','account').includes('未能确认'));
 assert(probeFeedback({class:'unknown',updated:false,buckets:[{id:'bot',class:'ok'}]},'grokbot','account').includes('查询完成'));
 assert(probeFeedback({tools:[{tool:'cursor',accounts:[{class:'ok'}]},{tool:'grokbot',accounts:[{class:'unknown'},{class:'expired'},{class:'ok',quota_stale:true},{class:'ok'}]}]},'grokbot','').includes('1 个账号已确认，3 个尚未确认（1 个认证已失效'));
 assert(probeFeedback({tools:[{tool:'cursor',accounts:[{class:'ok'}]}]},'grokbot','').includes('未能确认'));
 assert(probeFeedback({tools:[{tool:'codex',accounts:[{class:'ok',last_probe_class:'unknown'}]}]},'codex','').includes('1 个尚未确认'));
 state.overview={tools:[{tool:'grokbot',derived_from:'cursor',accounts:[{stable_id:'account'}]}]};
 let sent,feedback;mutate=async(t,label,path,body,done)=>{sent=body;feedback=done(bot('unknown'))};
 let button={disabled:false,dataset:{act:'probe',tool:'grokbot',id:'account'}};
 await onClick({target:{closest:s=>s==='[data-act]'?button:null}});assert.equal(sent.tool,'cursor');assert.equal(sent.id,'account');assert(feedback.includes('未能确认'));
 mutate=async(t,label,path,body,done)=>{sent=body;feedback=done({tools:[{tool:'cursor',accounts:[{class:'ok'}]},{tool:'grokbot',accounts:[{class:'unknown'}]}]})};
 button.dataset.act='probe-tool';await onClick({target:{closest:s=>s==='[data-act]'?button:null}});assert.equal(sent.id,'');assert(feedback.includes('1 个尚未确认'));
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}
