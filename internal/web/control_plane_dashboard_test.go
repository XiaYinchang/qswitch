package web

import "testing"

const controlPlaneFixtures = environmentFixtures + `
const report=(id,at=100,stale=false,value=overview(id))=>({overview:value,observed_at:value?at:0,received_at:value?at+1:0,stale});
const controlCatalog=(at=100)=>({instance:info(A),login_required:true,control_plane:true,server_time:at+1,report_max_age_seconds:60,environments:entries.map(env=>({...env,snapshot:report(env.id,at)}))});
const startPortal=async(catalog=controlCatalog())=>{state.gateway=null;state.envID='';state.apiBase='';state.online=false;state.overview=null;setFetch(async(path)=>{assert.equal(path,'api/environments');return response(catalog)});await bootstrap()};
const quotaCatalog=(at=100,used=0)=>{
 const catalog=controlCatalog(at),tool=catalog.environments[1].snapshot.overview.tools[0];
 tool.capabilities.probe={available:true};
 Object.assign(tool.accounts[0],{tool:'antigravity',live:true,class:'ok',used_pct:used,last_quota_at:at,last_probe_at:at,last_probe_class:'ok',quota_stale:false,
  buckets:[{id:'gemini_5h',used_pct:0},{id:'gemini_weekly',used_pct:0},{id:'third_party_5h',used_pct:used},{id:'third_party_weekly',used_pct:used}]});
 return catalog;
};
const clickRefresh=()=>onClick({target:{closest:selector=>selector==='[data-act]'?$('refreshState'):null}});
`

func TestDashboardPortalTabsAndPollingOnlyReadCentralSnapshots(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 const calls=[];setFetch(async(path,opts)=>{calls.push(path);assert.equal(path,'api/environments');return response(controlCatalog())});
 state.gateway=null;state.envID='';state.apiBase='';await bootstrap();assert.equal(calls.length,1);assert.equal(state.controlPlane,true);
 const cache=state.environments;
 for(const id of [B,A,B]){await switchEnvironment(id);assert.equal(state.instance.id,id);assert.equal(canWrite(),true)}
 assert.equal(calls.length,1,'tabs must not issue target GETs or re-read the catalog');assert.equal(state.environments,cache);
 await pollOverview();assert.equal(calls.length,2);assert.equal(state.envID,B);assert.equal(state.snapshotObservedAt,100);assert.equal(state.lastRead,101);
 const pending=deferred();let signal;setFetch((path,opts)=>{assert.equal(path,'api/environments');signal=opts.signal;return pending.promise});
 const refresh=loadCatalog();await switchEnvironment(A);assert.equal(signal.aborted,false,'tab changes must not cancel shared catalog reads');
 pending.resolve(response(controlCatalog(200)));await refresh;assert.equal(state.instance.id,A);assert.equal(state.lastRead,201);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalFailureKeepsHistoryAndRestoresOnReport(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal();await switchEnvironment(B);const previous=state.overview,cache=state.environments;
 setFetch(async()=>{throw new TypeError('control plane unavailable')});assert.equal(await loadCatalog(),false);
 assert.equal(state.overview,previous);assert.equal(state.environments,cache);assert.equal(canWrite(),false);assert.equal(state.snapshotStale,true);assert.equal(state.lastRead,101);assert.equal(state.connecting,false);
 assert($('accountDetail').innerHTML.includes('beta@example.test'));assert($('globalNotice').textContent.includes('收到上报'));assert.equal($('refreshState').disabled,false);
 let writes=0;setFetch(async()=>{writes++;return response({})});assert.equal(await mutate('antigravity','切号','api/switch',{}),false);assert.equal(writes,0);
 await switchEnvironment(A);assert.equal(state.instance.id,A);assert.equal(canWrite(),false);assert.equal(writes,0);
 setFetch(async(path)=>{assert.equal(path,'api/environments');return response(controlCatalog(200))});assert.equal(await loadCatalog(),true);
 assert.equal(canWrite(),true);assert.equal(state.controlError,'');assert.equal(state.lastRead,201);assert.equal(state.envID,A);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalRejectsMixedIdentitiesAndNeverFallsBack(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal();const original=state.overview;
 const wrong=controlCatalog(200);wrong.environments[1].snapshot.overview=overview(A);
 let calls=[];setFetch(async(path)=>{calls.push(path);assert.equal(path,'api/environments');return response(wrong)});
 assert.equal(await loadCatalog(),false);assert.equal(state.overview,original);assert.equal(canWrite(),false);assert(state.controlError.includes('实例不匹配'));
 const fallback={instance:info(A),login_required:true,control_plane:false,environments:[{id:A,name:'Local',api_base:'/qswitch/api/'}]};
 setFetch(async(path)=>{calls.push(path);assert.equal(path,'api/environments');return response(fallback)});assert.equal(await loadCatalog(),false);assert.equal(state.controlPlane,true);assert.equal(state.overview,original);assert.equal(canWrite(),false);
 assert.equal(calls.length,2);
 setFetch(async()=>response(controlCatalog(300)));assert.equal(await loadCatalog(),true);assert.equal(canWrite(),true);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalCanRefreshMissingOrStaleReports(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 for(const missing of [true,false]){
  const catalog=controlCatalog();catalog.environments[1].snapshot=report(B,100,true,missing?null:overview(B));
  await startPortal(catalog);await switchEnvironment(B);assert.equal(canWrite(),false);assert.equal(canRefreshReport(),true);assert.equal($('refreshState').disabled,false);
  if(missing)assert($('root').innerHTML.includes('等待节点上报'));else assert($('accountDetail').innerHTML.includes('beta@example.test'));
  const calls=[];setFetch(async(path,opts)=>{calls.push({path,method:opts.method});if(opts.method==='POST'){assert.equal(path,entries[1].api_base+'report');assert.equal(opts.headers['X-Qswitch-Instance'],B);return response({instance:info(B),reported:true,observed_at:200})}assert.equal(path,'api/environments');return response(controlCatalog(200))});
  await assert.rejects(api('api/switch',{method:'POST',body:{}}));assert.equal(calls.length,0,'only the exact report collection action may bypass stale-data gating');
  assert.equal(await refreshReportedSnapshot(),true);assert.deepEqual(calls.map(c=>c.method),['POST','GET']);assert.equal(state.instance.id,B);assert.equal(state.lastRead,201);assert.equal(canWrite(),true);
  clearTimeout(ui.overviewTimer);
 }
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalRefreshWaitsForReportThenReadsCentralAgain(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal();await switchEnvironment(B);
 const old=deferred(),ack=deferred(),fresh=deferred(),calls=[];let reads=0;
 setFetch((path,opts)=>{calls.push({path,opts});if(opts.method==='POST'){assert.equal(path,entries[1].api_base+'report');return ack.promise}assert.equal(path,'api/environments');return ++reads===1?old.promise:fresh.promise});
 const polling=loadCatalog(),refreshing=refreshReportedSnapshot();assert.equal(calls.length,2);assert.equal($('refreshState').disabled,true);assert.equal(state.lastRead,101);
 ack.resolve(response({instance:info(B),reported:true,observed_at:200}));await settle();assert.equal(calls.length,3);assert(calls[0].opts.signal.aborted,'the acknowledged report must not reuse a pre-ack catalog request');
 fresh.resolve(response(controlCatalog(200)));assert.equal(await refreshing,true);await polling;assert.equal(state.lastRead,201);assert.equal($('refreshState').disabled,false);
 old.resolve(response(controlCatalog(50)));await settle();assert.equal(state.lastRead,201);
 setFetch(async()=>response({error:'node unavailable',result_unknown:true},503));assert.equal(await refreshReportedSnapshot(),false);assert(state.overview);assert.equal(canWrite(),false);assert.equal($('refreshState').disabled,false);assert($('globalNotice').textContent.includes('刷新失败'));
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalMutationDoesNotPretendOldReportIsNew(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal();const calls=[];let successFeedback=0;
 setFetch(async(path,opts)=>{calls.push({path,method:opts.method});if(opts.method==='POST'){assert.equal(path,entries[0].api_base+'capture');return response({captured:['new-account']})}assert.equal(path,'api/environments');return response(controlCatalog())});
 assert.equal(await mutate('codex','收录','api/capture',{tool:'codex'},()=>{successFeedback++;return 'fresh result'}),true);
 assert.equal(successFeedback,0);assert.equal(state.lastRead,101);assert.equal(state.snapshotObservedAt,100);assert.equal(state.overview.tools[0].accounts.length,1);assert(state.messages.get('codex').text.includes('状态将在下次上报后更新'));
 assert.deepEqual(calls.map(c=>c.method),['POST','GET']);
 const updated=controlCatalog(200);updated.environments[0].snapshot.overview.tools[0].accounts.push({stable_id:'new-account',email:'new@example.test'});
 setFetch(async(path)=>{assert.equal(path,'api/environments');return response(updated)});await pollOverview();assert.equal(state.overview.tools[0].accounts.length,2);assert.equal(state.lastRead,201);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalDeadlinePreservesCacheAndAllowsRetry(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal();const original=api;api=(path,options={})=>original(path,{...options,timeout:2});
 setFetch(()=>new Promise(()=>{}));assert.equal(await loadCatalog(),false);assert.equal(state.connecting,false);assert(state.overview);assert.equal(canWrite(),false);assert.equal($('refreshState').disabled,false);
 assert.equal(await refreshReportedSnapshot(),false);assert.equal($('refreshState').disabled,false);assert.equal(state.reportRequests.size,0);
 setFetch(async(path,opts)=>opts.method==='POST'?response({instance:info(A),reported:true,observed_at:200}):response(controlCatalog(200)));
 assert.equal(await refreshReportedSnapshot(),true);assert.equal(canWrite(),true);assert.equal(state.lastRead,201);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalExpiresDuringSlowCatalogRead(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal();assert.equal(canWrite(),true);
 const pending=deferred();setFetch(()=>pending.promise);const reading=loadCatalog();
 advanceClock(59000);assert.equal(canWrite(),false,'an in-flight catalog read must not extend cached status lifetime');
 assert.equal(snapshotState(state.environments[0]),'历史状态');render();assert.equal(state.snapshotStale,true);assert($('globalNotice').textContent.includes('历史状态'));assert.equal($('refreshState').disabled,false);
 let posts=0;setFetch(async()=>{posts++;return response({})});await assert.rejects(api('api/switch',{method:'POST',body:{tool:'codex'}}));assert.equal(posts,0);
 pending.resolve(response(controlCatalog(200)));await reading;
 assert.equal(canWrite(),false,'request transit time must be charged even when the catalog itself carries a fresh server timestamp');
 setFetch(async()=>response(controlCatalog(300)));await loadCatalog();assert.equal(canWrite(),true);
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalAgeIgnoresClientOffsetAndUsesEarlierReportTime(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 advanceWall(864000000);await startPortal();assert.equal(canWrite(),true,'a fixed client clock offset must not make fresh server data stale');
 advanceClock(58000);assert.equal(canWrite(),true);advanceClock(1000);assert.equal(canWrite(),false);
 const catalog=controlCatalog(150);catalog.server_time=160;catalog.environments[0].snapshot.received_at=101;
 setFetch(async()=>response(catalog));await loadCatalog();assert.equal(canWrite(),true);advanceClock(1000);assert.equal(canWrite(),false,'age must use the earlier observed/received timestamp');
 setFetch(async()=>response(controlCatalog(300)));await loadCatalog();assert.equal(canWrite(),true);
 advanceWall(61000);assert.equal(canWrite(),false,'sleep with a frozen monotonic clock must require revalidation');render();assert.equal(snapshotState(state.environments[0]),'历史状态');
 setFetch(async()=>response(controlCatalog(400)));await loadCatalog();assert.equal(canWrite(),true);
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalResumeRequiresNewCatalog(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal();const old=deferred();setFetch(()=>old.promise);const beforeSleep=loadCatalog();
 document.hidden=true;await refreshAfterResume();assert.equal(canWrite(),false);
 const fresh=deferred();let calls=0;setFetch((path)=>{calls++;assert.equal(path,'api/environments');return fresh.promise});
 document.hidden=false;const resumed=refreshAfterResume();assert.equal(calls,1);assert.equal(canWrite(),false,'frozen clocks must not restore write access on visibility alone');
 fresh.resolve(response(controlCatalog(200)));await resumed;await beforeSleep;assert.equal(canWrite(),true);assert.equal(state.lastRead,201);
 old.resolve(response(controlCatalog(50)));await settle();assert.equal(state.lastRead,201);
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalRefreshRequiresTheAcknowledgedObservation(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal();setFetch(async(path,opts)=>opts.method==='POST'?response({instance:info(A),reported:true,observed_at:200}):response(controlCatalog()));
 assert.equal(await refreshReportedSnapshot(),false);assert.equal(state.lastRead,101);assert.equal(canWrite(),false);assert($('globalNotice').textContent.includes('尚未读到本次上报'));
 setFetch(async()=>response(controlCatalog(150)));await loadCatalog();assert.equal(canWrite(),false,'a report older than the acknowledged observation must not release the pending refresh');
 setFetch(async()=>response(controlCatalog(200)));await loadCatalog();assert.equal(canWrite(),true);assert.equal(state.lastRead,201);assert(!$('globalNotice').textContent.includes('尚未读到本次上报'));
 for(const observed of [undefined,0,1.5,'200']){
  let catalogs=0;setFetch(async(path,opts)=>{if(opts.method==='POST')return response({instance:info(A),reported:true,observed_at:observed});catalogs++;return response(controlCatalog(300))});
  assert.equal(await refreshReportedSnapshot(),false);assert.equal(catalogs,0,'invalid acknowledgements must not be presented as confirmed reports');assert.equal($('refreshState').disabled,false);
 }
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalZCodeCaptureErrorClearsOnRecoveredReport(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 const account={tool:'zcode',stable_id:'bigmodel:recovery-fixture',email:'recovery@example.test',plan:'Max',live:true,class:'ok',used_pct:37,buckets:[{id:'5h',used_pct:37,resets_at:500}]};
 const failure='无法读取 ZCode 当前桌面账号信息';
 const zcode={tool:'zcode',live_id:account.stable_id,capture_error:failure,capabilities:{capture:{available:true},probe:{available:true},switch:{available:false},auto_switch:{available:false}},accounts:[account]};
 const first=controlCatalog(100);first.environments[0].snapshot.overview={instance:{...info(A),managed_tools:['zcode']},tools:[zcode]};
 location.hash='#zcode';await startPortal(first);assert.equal(state.view,'zcode');
 assert.equal($('toolProblem').hidden,false);assert($('toolProblem').textContent.includes(failure));assert($('accountDetail').innerHTML.includes('recovery@example.test'));assert($('accountDetail').innerHTML.includes('37%'));
 const selected=state.selected.get('zcode');
 const recovered=controlCatalog(200),current=JSON.parse(JSON.stringify(zcode));delete current.capture_error;
 recovered.environments[0].snapshot.overview={instance:{...info(A),managed_tools:['zcode']},tools:[current]};
 let reads=0;setFetch(async(path,opts)=>{reads++;assert.equal(path,'api/environments');assert.equal(opts.method,'GET');return response(recovered)});
 assert.equal(await loadCatalog(),true);assert.equal(reads,1);
 assert.equal($('toolProblem').hidden,true);assert.equal($('toolProblem').textContent,'');
 assert.equal(state.selected.get('zcode'),selected);assert.equal(state.overview.tools[0].live_id,account.stable_id);assert.equal(state.overview.tools[0].accounts.length,1);assert.equal(state.overview.tools[0].accounts[0].live,true);
 assert($('accountDetail').innerHTML.includes('recovery@example.test'));assert($('accountDetail').innerHTML.includes('37%'));assert.equal(state.lastRead,201);
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalHeaderRefreshQueriesCurrentToolAndDisplaysReportedQuota(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal(quotaCatalog());await switchEnvironment(B);
 const button=$('refreshState');button.dataset={act:'refresh-state'};
 const ack=deferred(),calls=[];
 setFetch((path,opts)=>{
  calls.push({path,opts});
  if(opts.method==='POST'){
   assert.equal(path,entries[1].api_base+'report');assert.equal(opts.headers['X-Qswitch-Instance'],B);
   assert.deepEqual(JSON.parse(opts.body),{tool:'antigravity'});return ack.promise;
  }
  assert.equal(path,'api/environments');return Promise.resolve(response(quotaCatalog(200,47)));
 });
 const refresh=clickRefresh(),duplicate=refreshReportedSnapshot(requestContext(),'antigravity');
 assert.equal(calls.length,1);assert.equal(button.disabled,true);assert($('globalNotice').textContent.includes('正在查询 Antigravity 额度并上报'));
 assert.equal(state.overview.tools[0].accounts[0].last_quota_at,100);
 ack.resolve(response({instance:info(B),reported:true,observed_at:200,quota:{tool:'antigravity',attempted:1,updated:1,failed:0,skipped:0}}));
 await refresh;assert.equal(await duplicate,true);assert.deepEqual(calls.map(c=>c.opts.method),['POST','GET']);
 assert.equal(state.overview.tools[0].accounts[0].last_quota_at,200);
 assert.equal(state.overview.tools[0].accounts[0].buckets[2].used_pct,47);assert($('accountDetail').innerHTML.includes('已用 47%'));
 assert($('globalNotice').textContent.includes('额度已更新并上报'));assert($('globalNotice').textContent.includes('状态上报'));
 assert(!$('globalNotice').textContent.includes('节点采集'));assert.equal(button.disabled,false);
 const reads=calls.length;await pollOverview();assert.equal(calls.length,reads+1);assert.equal(calls.at(-1).opts.method,'GET','periodic polling must not query provider quota');
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalHeaderRefreshHandlesDerivedUnavailableAndManagementViews(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal(quotaCatalog());await switchEnvironment(B);$('refreshState').dataset={act:'refresh-state'};
 for(const mode of ['derived','unavailable','management','missing']){
  const catalog=quotaCatalog(200);
  if(mode==='derived'){
   const cursor=catalog.environments[1].snapshot.overview.tools[0];cursor.tool='cursor';
   catalog.environments[1].snapshot.overview.instance.managed_tools=['cursor'];
   catalog.environments[1].snapshot.overview.tools.push({...cursor,tool:'grokbot',derived_from:'cursor'});
  }
  if(mode==='unavailable')catalog.environments[1].snapshot.overview.tools[0].capabilities.probe={available:false};
  if(mode==='missing')catalog.environments[1].snapshot=report(B,0,true,null);
  setFetch(async()=>response(catalog));await loadCatalog();
  if(mode==='derived')selectView('grokbot');else if(mode==='management')selectView('environments');else if(mode!=='missing')selectView('antigravity');
  const calls=[];
  setFetch(async(path,opts)=>{
   calls.push({path,opts});
   if(opts.method==='POST'){
    const body=JSON.parse(opts.body);assert.deepEqual(body,mode==='derived'?{tool:'cursor'}:{});
    return response({instance:info(B),reported:true,observed_at:200,...(mode==='derived'?{quota:{tool:'cursor',attempted:1,updated:1,failed:0,skipped:0}}:{})});
   }
   assert.equal(path,'api/environments');return response(catalog);
  });
  await clickRefresh();assert.equal(calls.filter(c=>c.opts.method==='POST').length,1);
 }
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalQuotaRefreshReportsFailuresAndSkippedQueries(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 for(const quota of [
  {tool:'antigravity',attempted:2,updated:1,failed:1,skipped:0},
  {tool:'antigravity',attempted:0,updated:0,failed:0,skipped:1},
  {tool:'antigravity',attempted:1,updated:0,failed:1,skipped:1},
 ]){
  await startPortal(quotaCatalog());await switchEnvironment(B);
  const snapshot=quotaCatalog(200);snapshot.environments[1].snapshot.overview.tools[0].accounts[0].last_quota_at=100;
  setFetch(async(path,opts)=>opts.method==='POST'?response({instance:info(B),reported:true,observed_at:200,quota}):response(snapshot));
  assert.equal(await refreshReportedSnapshot(requestContext(),'antigravity'),false);
  assert.equal(state.lastRead,201);assert.equal(canWrite(),true,'partial quota failure must not imply an offline node');
  const notice=state.reportMessages.get(B);assert.equal(notice.error,true);assert(!notice.text.includes('额度已更新并上报'));
  if(quota.failed)assert(notice.text.includes('失败 '+quota.failed+' 个'));
  if(quota.skipped)assert(notice.text.includes('跳过 '+quota.skipped+' 个（未重新查询）'));
  assert.equal(state.overview.tools[0].accounts[0].last_quota_at,100,'report acknowledgement must not fabricate a new quota timestamp');
 }
 await startPortal(quotaCatalog());await switchEnvironment(B);
 setFetch(async(path,opts)=>opts.method==='POST'?response({instance:info(B),reported:true,observed_at:200,quota:{tool:'antigravity',attempted:0,updated:0,failed:0,skipped:0}}):response(quotaCatalog(200)));
 assert.equal(await refreshReportedSnapshot(requestContext(),'antigravity'),true);assert(state.reportMessages.get(B).text.includes('暂无可查询'));
 for(const quota of [undefined,{tool:'codex',attempted:1,updated:1,failed:0,skipped:0},{tool:'antigravity',attempted:1,updated:'1',failed:0,skipped:0},{tool:'antigravity',attempted:1,updated:1,failed:-1,skipped:0},{tool:'antigravity',attempted:1,updated:1,failed:1,skipped:0}]){
  await startPortal(quotaCatalog());await switchEnvironment(B);let reads=0;
  setFetch(async(path,opts)=>{if(opts.method==='POST')return response({instance:info(B),reported:true,observed_at:200,quota});reads++;return response(quotaCatalog(200))});
  assert.equal(await refreshReportedSnapshot(requestContext(),'antigravity'),false);assert.equal(reads,0);
  assert.equal(state.reportMessages.get(B).error,true);
 }
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalProbeImmediatelyPublishesWithoutRepeatingQuotaQuery(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 for(const single of [true,false]){
  await startPortal(quotaCatalog());await switchEnvironment(B);
  const newer=quotaCatalog(200,58),id=single?B+'-account':'',ack=deferred(),calls=[];
  setFetch((path,opts)=>{
   calls.push({path,opts});
   if(path.endsWith('/probe'))return Promise.resolve(response(single?{updated:true,class:'ok',used_pct:58}:newer.environments[1].snapshot.overview));
   if(path.endsWith('/report')){assert.deepEqual(JSON.parse(opts.body),{});return ack.promise}
   assert.equal(path,'api/environments');return Promise.resolve(response(newer));
  });
  const probing=mutate('antigravity','查询用量','api/probe',{tool:'antigravity',id},r=>probeFeedback(r,'antigravity',id));
  await settle();assert.equal(calls.length,2);assert.equal(state.overview.tools[0].accounts[0].last_quota_at,100);
  ack.resolve(response({instance:info(B),reported:true,observed_at:200}));assert.equal(await probing,true);
  assert.deepEqual(calls.map(c=>c.path),[entries[1].api_base+'probe',entries[1].api_base+'report','api/environments']);
  assert.equal(state.overview.tools[0].accounts[0].last_quota_at,200);assert($('accountDetail').innerHTML.includes('已用 58%'));
  const text=state.messages.get('antigravity').text;assert(!text.includes('下次上报'));assert(!text.includes('控制面暂未更新'));
  if(single)assert(text.includes('查询完成'));else assert(text.includes('请以各账号“用量记录”时间为准'));
 }
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalProbeRequiresReportAfterCompletionAndKeepsEnvironmentPin(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal(quotaCatalog());await switchEnvironment(B);
 const before=deferred(),after=deferred(),calls=[];let reports=0,at=100;
 setFetch((path,opts)=>{
  calls.push({path,opts});
  if(path.endsWith('/report')){assert.equal(opts.headers['X-Qswitch-Instance'],B);assert.deepEqual(JSON.parse(opts.body),{});return ++reports===1?before.promise:after.promise}
  if(path.endsWith('/probe'))return Promise.resolve(response({updated:true,class:'ok',used_pct:69}));
  assert.equal(path,'api/environments');return Promise.resolve(response(quotaCatalog(at,at===300?69:0)));
 });
 const earlier=refreshReportedSnapshot(),probe=mutate('antigravity','查询用量','api/probe',{tool:'antigravity',id:B+'-account'},r=>probeFeedback(r,'antigravity',B+'-account'));
 await settle();assert.equal(reports,1,'report commands for one environment must remain serialized');
 at=200;before.resolve(response({instance:info(B),reported:true,observed_at:200}));await earlier;await settle();assert.equal(reports,2,'a report started before probe completion cannot acknowledge the probe');
 assert.equal(state.overview.tools[0].accounts[0].used_pct,0);
 await switchEnvironment(A);assert.equal(state.instance.id,A);assert.equal(state.messages.size,0);
 at=300;after.resolve(response({instance:info(B),reported:true,observed_at:300}));assert.equal(await probe,false);
 assert.equal(state.instance.id,A);assert.equal(state.messages.size,0);assert($('accountDetail').innerHTML.includes('alpha@example.test'));
 assert(!calls.some(c=>c.opts.method==='POST'&&c.path.startsWith(entries[0].api_base)),'late work must never target the newly selected environment');
 assert.equal(state.reportRequests.size,0);
 await switchEnvironment(B);assert.equal(state.overview.tools[0].accounts[0].used_pct,69);
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardPortalProbeDoesNotClaimSuccessBeforeConfirmedReport(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 await startPortal(quotaCatalog());await switchEnvironment(B);let feedback=0;
 setFetch(async(path,opts)=>{
  if(path.endsWith('/probe'))return response({updated:true,class:'ok',used_pct:81});
  if(path.endsWith('/report'))return response({instance:info(B),reported:true,observed_at:200});
  return response(quotaCatalog());
 });
 assert.equal(await mutate('antigravity','查询用量','api/probe',{tool:'antigravity',id:B+'-account'},()=>{feedback++;return '查询完成'}),true);
 assert.equal(feedback,0);assert.equal(state.messages.get('antigravity').error,true);
 assert(state.messages.get('antigravity').text.includes('尚未确认本次结果'));assert.equal(state.overview.tools[0].accounts[0].last_quota_at,100);
 assert(probeFeedback({updated:false,class:'ok',source:'backoff'},'antigravity',B+'-account').includes('本次未重新查询'));
 clearTimeout(ui.overviewTimer);clearTimeout(ui.snapshotExpiryTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}
