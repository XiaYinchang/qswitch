package web

import "testing"

func TestDashboardConnectionSharesPendingOverview(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 const pending=deferred(),calls=[];
 setFetch((path,opts)=>{calls.push({path,opts});return pending.promise});
 const changing=switchEnvironment(B);
 const reconnect=connect(),poll=pollOverview(),read=loadSnapshot();
 assert.equal(calls.length,1,'concurrent connection, visibility poll and refresh must share one read');
 assert.equal(calls[0].path,entries[1].api_base+'overview');assert.equal(calls[0].opts.headers['X-Qswitch-Instance'],B);
 assert.equal(canWrite(),false);assert.equal(state.trustedID,'');assert.equal(state.overview,null);
 pending.resolve(response(overview(B)));await Promise.all([changing,reconnect,poll,read]);
 assert.equal(canWrite(),true);assert.equal(state.trustedID,B);assert.equal(state.instance.id,B);
 assert.equal(calls[0].opts.signal.aborted,false,'a duplicate read must not cancel useful work');
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardCurrentEnvironmentDoesNotReconnect(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 const epoch=state.epoch,original=state.overview;let reads=0;
 original.tools[0].accounts.push({stable_id:'selected-account',email:'selected@example.test'});
 state.selected.set('codex','selected-account');
 setFetch(async()=>{reads++;return response(overview(A))});
 await switchEnvironment(A);
 assert.equal(reads,0);assert.equal(state.epoch,epoch);assert.equal(state.overview,original);assert.equal(state.selected.get('codex'),'selected-account');
 assert.equal(canWrite(),true);
 const button=$('refreshState');button.dataset={act:'refresh-state'};
 await onClick({target:{closest:s=>s==='[data-act]'?button:null}});
 assert.equal(reads,1,'an explicit refresh still fetches one fresh overview');
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardMutationRequiresNewSnapshot(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 const before=deferred(),during=deferred(),after=deferred(),write=deferred();
 const reads=[],posts=[];
 setFetch((path,opts)=>{
  if(opts.method==='POST'){posts.push(path);return write.promise}
  reads.push(opts);return [before,during,after][reads.length-1].promise;
 });
 const oldRead=loadSnapshot();
 const mutation=mutate('codex','收录','api/capture',{tool:'codex'});
 const midRead=loadSnapshot(),sameMidRead=loadSnapshot();
 assert.equal(reads.length,2);assert(reads[0].signal.aborted);assert.equal(reads[1].signal.aborted,false);
 write.resolve(response({captured:['synthetic']}));await settle();
 assert.equal(posts.length,1);assert.equal(reads.length,3,'write completion must supersede the in-flight pre-completion snapshot');assert(reads[1].signal.aborted);
 after.resolve(response(overview(A,'after write')));assert.equal(await mutation,true);
 before.resolve(response(overview(A,'before write')));during.resolve(response(overview(A,'during write')));
 await Promise.all([oldRead,midRead,sameMidRead]);
 assert.equal(state.instance.name,'after write');assert.equal(canWrite(),true);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardOverviewMustVerifyIdentityAndCompleteState(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 for(const payload of [overview(A),{instance:info(B)}]){
  let reads=0;setFetch(async(path,opts)=>{reads++;assert.equal(path,entries[1].api_base+'overview');assert.equal(opts.headers['X-Qswitch-Instance'],B);return response(payload)});
  await switchEnvironment(B);
  assert.equal(reads,1);assert.equal(canWrite(),false);assert.equal(state.trustedID,'');assert.equal(state.instance,null);assert.equal(state.overview,null);
 }
 setFetch(normalFetch);await connect();assert.equal(canWrite(),true);assert.equal(state.fatal,'');assert.equal(state.trustedID,B);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardCatalogSuppliesLoginState(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 for(const required of [true,false,undefined]){
  state.gateway=null;state.envID='';state.apiBase='';state.online=false;state.overview=null;
  location.href='https://manage.example.test/qswitch/';const calls=[];
  setFetch(async(path,opts)=>{calls.push(path);if(path==='api/environments')return response({instance:info(A),environments:entries,login_required:required});assert.equal(path,entries[0].api_base+'overview');return normalFetch(path,opts)});
  await bootstrap();
  if(required===undefined){assert.equal(calls.length,1);assert.equal(canWrite(),false);assert(state.fatal.includes('登录状态缺失'))}
  else{assert.equal(calls.length,2);assert.equal(canWrite(),true);assert.equal($('logoutForm').hidden,!required)}
  clearTimeout(ui.overviewTimer);
 }
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardUnavailableBlockersNeverAdvertiseIdle(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
const tool=state.overview.tools[0];tool.blockers_unavailable=true;tool.busy=true;state.view='codex';
render();assert($('toolStatus').innerHTML.includes('会话状态未知'));assert(!$('toolStatus').innerHTML.includes('会话使用中'));assert(!$('toolStatus').innerHTML.includes('会话空闲'));
tool.blockers_unavailable=false;render();assert($('toolStatus').innerHTML.includes('会话使用中'));
tool.busy=false;render();assert($('toolStatus').innerHTML.includes('会话空闲'));
`)
}
