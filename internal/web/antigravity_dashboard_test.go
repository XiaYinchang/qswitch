package web

import "testing"

func TestAntigravityDashboardQuotaAndSwitch(t *testing.T) {
	dashboardScript(t, `
const ids=['gemini_5h','gemini_weekly','third_party_5h','third_party_weekly'];
const base={tool:'antigravity',stable_id:'synthetic-account',email:'fixture@example.test',class:'ok',used_pct:40,buckets:ids.map((id,i)=>({id,used_pct:[100,20,40,30][i]}))};
const tool={tool:'antigravity',capabilities:{capture:{available:true},probe:{available:true},switch:{available:true},login:{available:false,command:'qswitch run antigravity'}}};
let html=accountHTML(base,tool);
assert(html.includes('Gemini'));assert(html.includes('第三方模型'));assert(html.includes('两组额度都耗尽'));
assert(html.includes('已用 100%'));assert(html.includes('已用 40%'));assert.equal(quotaClass(base),'ok');
for(let missing=0;missing<4;missing++){
 const ac={...base,buckets:base.buckets.filter((_,i)=>i!==missing)};
 assert.equal(quotaClass(ac),'unknown');html=accountHTML(ac,tool);
 assert(html.includes('用量未知'));assert(html.includes('暂不自动切换'));assert.equal(quotaBuckets(ac).length,4);
}

html=bucketHTML({...base,class:'unknown',buckets:[]});
assert.equal((html.match(/class="bar empty"/g)||[]).length,4);assert(!html.includes('已用 0%'));
html=accountHTML({...base,quota_stale:true,last_probe_class:'unknown',last_quota_at:1790000000,last_probe_at:1790003600},tool);
assert(html.includes('显示历史用量'));assert(html.includes('已用 40%'));assert(!accountStatus({...base,quota_stale:true}).includes('pill ok'));
assert.equal(usedText(99.99,'antigravity'),'已用 99%');
const note=switchNote({tool:'antigravity',busy:true,pending_to:'synthetic-target',accounts:[]});
assert(note.includes('等待桌面 App 和 CLI 会话空闲'));assert(note.includes('再次核验两组额度'));assert(!note.includes('重启'));
assert(loginGuidance(tool).includes('qswitch run antigravity'));
assert(switchImpact('antigravity').includes('不重启客户端'));assert(!switchImpact('antigravity').includes('无缝'));
`)
}

func TestAntigravityUnknownQuotaEndpointHasSpecificGuidance(t *testing.T) {
	dashboardScript(t, `
const notice='无法确认 Antigravity 的额度服务入口；请启动已配置的客户端后刷新。';
const account={tool:'antigravity',stable_id:'endpoint-fixture',email:'endpoint@example.test',class:'unknown',quota_stale:true,last_probe_class:'unknown',last_probe_source:'endpoint_unknown',last_probe_at:1790003600,last_quota_at:1790000000,buckets:[]};
const tool={tool:'antigravity',capabilities:{probe:{available:true},switch:{available:true}}};
assert(quotaNote(account).includes(notice));
const html=accountHTML(account,tool);assert(html.includes(notice));assert(html.includes('暂不自动切换'));
assert(!html.includes('已用 0%'));assert(!html.includes('认证已失效'));assert(!html.includes('请重新登录'));
assert(html.includes('<dt>查询来源</dt><dd>'+notice+'</dd>'));
assert.equal(probeFeedback({class:'unknown',updated:false,source:'endpoint_unknown'},'antigravity','endpoint-fixture'),notice);
assert(probeFeedback({tools:[{...tool,accounts:[account]}]},'antigravity','').includes(notice));
assert(!quotaNote({...account,last_probe_source:'http'}).includes(notice));
assert(!quotaNote({...account,tool:'codex'}).includes(notice));
assert(!probeFeedback({class:'unknown',updated:false,source:'http'},'antigravity','endpoint-fixture').includes(notice));
`)
}
