package web

import "testing"

const environmentFixtures = `
const A='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', B='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';
const entries=[{id:A,name:'Alpha',api_base:'/qswitch/api/instances/'+A+'/'},{id:B,name:'Beta',api_base:'/qswitch/api/instances/'+B+'/'}];
const info=(id,name=id===A?'Alpha':'Beta')=>({id,name,managed_tools:id===A?['codex']:['antigravity']});
const overview=(id,name)=>({instance:info(id,name),tools:[{tool:id===A?'codex':'antigravity',accounts:[{stable_id:id+'-account',email:id===A?'alpha@example.test':'beta@example.test'}],capabilities:{capture:{available:true},login:{available:id===A}}}]});
const response=(data,status=200,headers={})=>({ok:status>=200&&status<300,status,headers:{get:key=>headers[key]||null},json:async()=>data});
const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b});return {promise,resolve,reject}};
const settle=async()=>{for(let i=0;i<12;i++)await Promise.resolve()};
const normalFetch=async(path,opts)=>{const id=path.includes(B)?B:A;assert.equal(opts.headers['X-Qswitch-Instance'],id);return response(overview(id))};
state.gateway=info(A);state.environments=entries;state.envID=A;state.apiBase=entries[0].api_base;state.trustedID=A;state.instance=info(A);state.online=true;state.overview=overview(A);
location.href='https://manage.example.test/qswitch/?env='+A+'#codex';
`

func TestDashboardEnvironmentReadEpochAndOffline(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 const late=deferred();setFetch(()=>late.promise);
 const oldRead=loadSnapshot();
 state.selected.set('codex','old-account');state.messages.set('codex',{text:'old message'});
 askConfirm({title:'切换',target:'old',impact:'',run:()=>{throw Error('stale confirmation ran')}});
 setFetch(normalFetch);await switchEnvironment(B);
 assert.equal($('refreshState').disabled,false);assert.equal(state.envID,B);assert.equal(state.instance.id,B);assert.equal(state.selected.has('codex'),false);assert.equal(state.messages.size,0);assert.equal(ui.confirm,null);assert.equal($('confirmDlg').open,false);
 assert.deepEqual([...ui.navNodes.keys()],['antigravity']);assert(!$('root').innerHTML.includes('alpha@example.test'));
 late.resolve(response(overview(A,'late A')));await oldRead;assert.equal(state.instance.id,B);
 const oldA=deferred();await switchEnvironment(A);setFetch(()=>oldA.promise);const firstA=loadSnapshot();
 setFetch(normalFetch);await switchEnvironment(B);await switchEnvironment(A);
 oldA.resolve(response(overview(A,'obsolete epoch')));await firstA;assert.equal(state.instance.name,'Alpha');
 setFetch(async()=>{throw new TypeError('offline')});await switchEnvironment(B);
 assert.equal(state.overview,null);assert.equal(state.instance,null);assert.equal(state.online,false);assert.equal(canWrite(),false);assert.equal(ui.navNodes.size,0);assert.equal(ui.accountNodes.size,0);
 assert($('root').innerHTML.includes('环境不可用'));assert(!$('root').innerHTML.includes('尚未添加账号'));assert.equal($('addAccount').disabled,true);assert.equal(location.hash,'#codex','offline must preserve the requested tool');
 let writes=0;setFetch(async()=>{writes++;return response({})});assert.equal(await mutate('codex','收录','api/capture',{}),false);assert.equal(writes,0);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardEnvironmentWritesStayBoundAndDeduplicate(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 const writeA=deferred(),writeB=deferred();let writes=[];
 setFetch((path,opts)=>{if(opts.method==='POST'){writes.push({path,id:opts.headers['X-Qswitch-Instance']});return path.includes(A)?writeA.promise:writeB.promise}return normalFetch(path,opts)});
 const first=mutate('codex','收录','api/capture',{tool:'codex'});
 await switchEnvironment(B);assert.equal(busy('codex'),false);
 const second=mutate('codex','收录','api/capture',{tool:'codex'});
 await switchEnvironment(A);assert.equal(busy('codex'),true);assert.equal(await mutate('codex','重复','api/capture',{}),false);assert.equal(writes.length,2);
 writeA.resolve(response({captured:['a']}));assert.equal(await first,false);assert.equal(state.messages.size,0);assert.equal(busy('codex'),false);
 await switchEnvironment(B);assert.equal(busy('codex'),true,'old A finally must not release B');
 writeB.resolve(response({captured:['b']}));assert.equal(await second,false);assert.equal(busy('codex'),false);assert.equal(state.messages.size,0);
 assert.equal(writes[0].id,A);assert(writes[0].path.startsWith(entries[0].api_base));assert.equal(writes[1].id,B);
 let ran=0;askConfirm({title:'x',target:'x',impact:'x',run:()=>ran++});const stale=ui.confirm;
 await switchEnvironment(A);ui.confirm=stale;
 await onClick({target:{closest:s=>s==='[data-act]'?{dataset:{act:'accept-confirm'}}:null}});assert.equal(ran,0);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardEnvironmentLoginStartAndCancelBinding(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 const start=deferred(),cancel=deferred();const calls=[];
 setFetch((path,opts)=>{calls.push({path,opts});if(path.endsWith('/login/codex'))return start.promise;if(path.endsWith('/cancel'))return cancel.promise;return normalFetch(path,opts)});
 const started=startLogin('codex');const original=ui.login;
 await switchEnvironment(B);assert.equal($('loginDlg').open,false);assert.equal($('loginCode').textContent,'');
 openAddAccount();assert.equal(ui.login.tool,'antigravity');assert(!$('addTool').innerHTML.includes('codex'));await cancelLogin();
 await switchEnvironment(A);assert.equal(busy('codex'),true);await startLogin('codex');assert.equal(calls.filter(c=>c.path.endsWith('/login/codex')).length,1);
 start.resolve(response({id:'login-alpha',url:'https://accounts.example.test/device',user_code:'ALPHA',expires_at:new Date(Date.now()+60000).toISOString()}));await settle();
 const cleanup=calls.find(c=>c.path.endsWith('/cancel'));assert(cleanup);assert.equal(cleanup.opts.headers['X-Qswitch-Instance'],A);assert(cleanup.path.startsWith(entries[0].api_base));assert.equal(JSON.parse(cleanup.opts.body).id,'login-alpha');assert.equal(busy('codex'),true);
 cancel.resolve(response({}));await started;assert.equal(busy('codex'),false);assert.notEqual($('loginCode').textContent,'ALPHA');assert.equal(original.timer,null);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardEnvironmentLoginPollAndDiagnosticsCannotCrossTabs(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 const poll=deferred();setFetch((path,opts)=>path.includes('/login/codex?')?poll.promise:normalFetch(path,opts));
 ui.login={mode:'device',generation:3,context:requestContext(),id:'alpha-login',tool:'codex',expires:Date.now()+60000,polling:false,timer:null};$('loginDlg').showModal();
 const pending=pollLogin(3);await switchEnvironment(B);
 poll.resolve(response({status:'ok'}));await pending;assert.equal(state.instance.id,B);assert.equal(state.messages.size,0);assert.equal($('loginDlg').open,false);assert.equal(ui.login.polling,true,'a stale finally must not mutate a new dialog');
 const diagnostics=deferred();setFetch(()=>diagnostics.promise);ui.tool='antigravity';
 const event={target:{closest:s=>s==='[data-act]'?{dataset:{act:'diagnostics',tool:'antigravity'}}:null}};
 const diagnosticRead=onClick(event);setFetch(normalFetch);await switchEnvironment(A);ui.tool='antigravity';$('diagnostics').hidden=true;
 diagnostics.resolve(response({diagnostics:'private old environment'}));await diagnosticRead;assert.equal($('diagnostics').hidden,true);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardEnvironmentBootstrapURLsAndAuthentication(t *testing.T) {
	dashboardScript(t, controlPlaneFixtures+`
(async()=>{
 state.gateway=null;state.envID='';state.apiBase='';state.expected=A;
 location.href='https://manage.example.test/qswitch/?expected_instance='+A+'&env='+B+'#antigravity';
 const calls=[];setFetch(async(path,opts)=>{calls.push({path,opts});assert.equal(path,'api/environments');return response(controlCatalog())});
 await bootstrap();assert.equal(state.envID,B);assert.equal(state.gateway.id,A);assert.equal(state.instance.id,B);assert.equal(location.pathname,'/qswitch/');assert.equal(new URL(location.href).searchParams.get('expected_instance'),A);
 assert.equal(calls.length,1,'portal startup only reads the central catalog');assert.equal(calls[0].opts.headers['X-Qswitch-Instance'],undefined);assert.equal($('logoutForm').hidden,false);
 const logout=new URL($('logoutForm').action,location.href);assert.equal(logout.pathname,'/qswitch/logout');assert.equal(logout.searchParams.get('env'),B);assert.equal(logout.searchParams.get('expected_instance'),A);
 location.href='https://manage.example.test/qswitch/?expected_instance='+A+'&env='+A+'#codex';await routeFromURL();assert.equal(state.envID,A);assert.equal(state.view,'codex');
 location.href='https://manage.example.test/qswitch/?env='+B+'#antigravity';await routeFromURL();assert.equal(state.envID,B);
 const count=calls.length;location.href='https://manage.example.test/qswitch/?env=unknown#codex';await routeFromURL();assert.equal(state.envID,'unknown');assert.equal(state.overview,null);assert(state.fatal.includes('未配置'));assert.equal(calls.length,count);assert.equal(location.hash,'#codex');
 location.href='https://manage.example.test/qswitch/#codex';await routeFromURL();assert.equal(state.envID,A);assert.equal(new URL(location.href).searchParams.get('env'),A);
 renderEnvironments();assert(!$('root').innerHTML.includes('/api/instances/'));assert(!$('root').innerHTML.includes('type="url"'));assert(!$('root').innerHTML.includes('保存目录'));assert($('root').innerHTML.includes('收到上报'));
 setFetch(async()=>response({},401));assert.equal(await loadCatalog(),false);assert.equal(canWrite(),false);assert(state.overview,'expired login retains an explicitly stale snapshot');
 assert(location.redirect.includes('/qswitch/login'));assert.equal(new URL(location.redirect).searchParams.get('expected_instance'),A);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardDirectEnvironmentAndInvalidCatalog(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 const cases=[{environments:[{id:A,name:'Local',api_base:'/api/'}],ok:true},{environments:[{id:B,name:'Wrong local',api_base:'/api/'}],ok:false},{environments:[{id:A,name:'External',api_base:'https://external.example.test/api/'}],ok:false},{environments:[entries[0],entries[0]],ok:false}];
 for(const fixture of cases){state.gateway=null;state.envID='';state.apiBase='';state.expected='';location.href='http://localhost:7432/';let targetCalls=0;
 setFetch(async(path,opts)=>{if(path==='api/environments')return response({instance:info(A),login_required:true,environments:fixture.environments});targetCalls++;assert(path.startsWith('/api/'));assert.equal(opts.headers['X-Qswitch-Instance'],A);return response(overview(A))});
 await bootstrap();assert.equal(canWrite(),fixture.ok);assert.equal(targetCalls,fixture.ok?1:0);clearTimeout(ui.overviewTimer);
 }
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardEnvironmentLateFailuresDoNotPoisonCurrentTab(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 for(const status of [401,409,503]){
  setFetch(normalFetch);await switchEnvironment(A);
  const late=deferred();setFetch(()=>late.promise);const old=loadSnapshot();
  setFetch(normalFetch);await switchEnvironment(B);
  late.resolve(response({error:'instance mismatch from old environment'},status,{'X-Qswitch-Login-Required':'1'}));await old;
  assert.equal(state.instance.id,B);assert.equal(canWrite(),true);assert.equal(state.fatal,'');assert.equal(location.redirect,undefined);
 }
 const superseded=deferred();setFetch(()=>superseded.promise);const sameEnvironment=loadSnapshot();
 setFetch(normalFetch);state.revision++;await loadSnapshot();superseded.resolve(response({error:'instance mismatch'},409));await sameEnvironment;assert.equal(canWrite(),true);assert.equal(state.fatal,'');
 const lateNetwork=deferred();setFetch(()=>lateNetwork.promise);const old=loadSnapshot();
 setFetch(normalFetch);await switchEnvironment(A);lateNetwork.reject(new TypeError('old connection lost'));await old;
 assert.equal(state.instance.id,A);assert.equal(canWrite(),true);assert.equal(state.connectionError,'');
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardCurrentShellPrefixAndBootstrapRetry(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 for(const prefix of ['/','/qswitch/','/qswitch-mini/','/custom/tools/']){
  state.gateway=null;state.envID='';state.apiBase='';state.expected='';location.href='https://manage.example.test'+prefix;
  const calls=[];setFetch(async(path,opts)=>{calls.push(path);if(path==='api/environments')return response({instance:info(A),login_required:true,environments:[{id:A,name:'Local',api_base:prefix+'api/'}]});assert.equal(opts.headers['X-Qswitch-Instance'],A);return response(overview(A))});
  await bootstrap();assert.equal(canWrite(),true);const logout=new URL($('logoutForm').action,location.href);assert.equal(logout.pathname,prefix+'logout');assert.equal(logout.searchParams.get('env'),A);assert.equal(logout.searchParams.get('expected_instance'),A);assert.deepEqual(calls,['api/environments',prefix+'api/overview']);clearTimeout(ui.overviewTimer);
 }
 location.href='https://manage.example.test/custom/tools/';state.gateway=null;let failCatalog=true;
 setFetch(async(path,opts)=>{if(path==='api/environments'){if(failCatalog)throw new TypeError('catalog temporarily unavailable');return response({instance:info(A),login_required:true,environments:[{id:A,name:'Local',api_base:'/custom/tools/api/'}]})}return response(overview(A))});
 await bootstrap();assert.equal(state.gateway,null);assert.equal(canWrite(),false);failCatalog=false;
 await onClick({target:{closest:s=>s==='[data-act]'?{dataset:{act:'refresh-state'}}:null}});assert.equal(canWrite(),true);clearTimeout(ui.overviewTimer);
 state.gateway=null;setFetch(async()=>response({instance:info(A),login_required:true,environments:entries}));await bootstrap();assert.equal(canWrite(),false,'a different shell prefix must be rejected');
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardLateRefreshDoesNotDisableNewEnvironment(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 setFetch(normalFetch);await switchEnvironment(A);
 const delayedTab=deferred();setFetch(()=>delayedTab.promise);const changing=switchEnvironment(B);assert($('root').innerHTML.includes('正在读取状态'));assert(!$('root').innerHTML.includes('id="workspace"'));
 setFetch(normalFetch);delayedTab.resolve(response(overview(B)));await changing;await switchEnvironment(A);
 const snapshot=deferred();setFetch(()=>snapshot.promise);
 const button=$('refreshState');button.dataset={act:'refresh-state'};
 const refresh=onClick({target:{closest:s=>s==='[data-act]'?button:null}});assert.equal(button.disabled,true);
 setFetch(normalFetch);await switchEnvironment(B);assert.equal(button.disabled,false);
 snapshot.resolve(response(overview(A)));await refresh;assert.equal(button.disabled,false);assert.equal(state.instance.id,B);
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardGatewayUnknownWriteOnlyRechecksState(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 let posts=0,reads=0;
 const failed=deferred();setFetch((path,opts)=>{if(opts.method==='POST'){posts++;return failed.promise}reads++;return normalFetch(path,opts)});
 const pending=mutate('codex','切换账号','api/switch',{tool:'codex',id:'synthetic-target'});
 failed.resolve(response({error:'目标连接中断；已提交的操作结果尚未确认，请刷新状态核对，不要重复提交',result_unknown:true},503));
 assert.equal(await pending,false);assert.equal(posts,1);assert.equal(reads,1);assert.equal(state.online,true);
 assert(state.messages.get('codex').text.includes('不要重复提交'));assert(state.messages.get('codex').error);
 setFetch(async()=>response({error:'读取失败',result_unknown:true},503));
 try{await api('api/overview');throw Error('expected failure')}catch(e){assert.equal(e.unknown,false)}
 assert(state.messages.get('codex').text.includes('不要重复提交'),'an offline re-read must retain the uncertain-write warning');
 clearTimeout(ui.overviewTimer);
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardBootstrap401PreservesRequestedEnvironment(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 for(const requested of [B,'','not-configured',null]){
  state.gateway=null;state.envID='';state.apiBase='';state.trustedID='';state.instance=null;state.online=false;state.overview=null;state.expected=A;
  const page=new URL('https://manage.example.test/qswitch/?expected_instance='+A+'#codex');
  if(requested!==null)page.searchParams.set('env',requested);
  location.href=page.href;location.redirect=undefined;
  const calls=[];setFetch(async(path,opts)=>{
   calls.push(path);assert.equal(path,'api/environments');return response({},401);
  });
  await bootstrap();
  assert(location.redirect,'authentication failure must redirect to login');
  const login=new URL(location.redirect);
  assert.equal(login.pathname,'/qswitch/login');assert.equal(login.hash,'#codex');
  assert.equal(login.searchParams.has('env'),requested!==null,'preserve absent versus empty env');
  assert.equal(login.searchParams.get('env'),requested,'bootstrap must not replace the requested environment with the gateway');
  assert.equal(login.searchParams.get('expected_instance'),A,'login identity must remain the gateway');
  assert.equal(state.envID,'');assert.equal(calls.length,1);
 }
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}

func TestDashboardSelectedEnvironmentTakesPriorityOn401(t *testing.T) {
	dashboardScript(t, environmentFixtures+`
(async()=>{
 state.envID=B;state.gateway=info(A);state.expected='';
 location.href='https://manage.example.test/qswitch/?env='+A+'#antigravity';
 setFetch(async()=>response({},401));
 await assert.rejects(api('api/session',{global:true}));
 const login=new URL(location.redirect);
 assert.equal(login.searchParams.get('env'),B);assert.equal(login.searchParams.get('expected_instance'),A);assert.equal(login.hash,'#antigravity');
})().catch(e=>{console.error(e);process.exitCode=1});
`)
}
