const {readFileSync} = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const {test} = require('node:test');
const page = readFileSync(__dirname + '/../internal/app/dashboard.html', 'utf8');
const code = page.split('<script>')[1].split('</script>')[0];
function fixture(initial = {}) {
  const f = {now:1800000000000, sample:1800000000000, failure:'', missing:false, state:'HEALTHY', repositories:['owner/repo'], fractions:[0.25,0.25,0.25,0.25], urls:[], ...initial};
  const classes = new Set(['expired']);
  const nodes = Object.fromEntries(['freshness','error','repos','range','window','custom','selection','from','to'].map(id => [id,{addEventListener(name,fn){this[name]=fn;},classList:{toggle(){}}}]));
  let details = [];
  let badges = [];
  let bars = [];
  const documentEvents = {}, windowEvents = {}, intervals = new Map();
  let html = '';
  Object.defineProperty(nodes.repos, 'innerHTML', {
    get:()=>html,
    set:value=>{ html=value;
      badges=[...value.matchAll(/class="runtime-badge">(.*?)<\/span>/g)].map(match=>({textContent:match[1]}));
      bars=[...value.matchAll(/class="bar timeline" data-from="(\d+)" data-to="(\d+)"/g)].map(match=>({
        dataset:{from:match[1],to:match[2]},
        getBoundingClientRect:()=>({left:100,width:1000}),
        closest(){return this;},append(overlay){this.overlay=overlay;},
        setPointerCapture(id){this.capture=id;},hasPointerCapture(id){return this.capture===id;},releasePointerCapture(){this.capture=null;},
      }));
      details=[...value.matchAll(/<details data-repository="([^"]+)"/g)].map(match=>({dataset:{repository:match[1]},open:false})); },
  });
  nodes.repos.querySelectorAll = selector => selector === '.runtime-badge' ? badges : details.filter(detail => selector !== 'details[open]' || detail.open);
  const context = vm.createContext({
    document:{createElement:()=>({style:{},remove(){this.removed=true;}}),body:{classList:{add:x=>classes.add(x),remove:x=>classes.delete(x)}},getElementById:x=>nodes[x],addEventListener(name,fn){documentEvents[name]=fn;}},
    window:{addEventListener(name,fn){windowEvents[name]=fn;}},Date:class extends Date {static now(){return f.now;}},
    performance:{now:()=>f.now},setInterval(fn,ms){intervals.set(ms,fn);},AbortSignal,URLSearchParams,
    fetch:async url => {
      f.urls.push(url);
      if (f.pause) await f.pause;
      if (f.failure && url.startsWith(f.failure)) throw new Error('offline');
      const u = new URL(url,'http://localhost');
      const from = Date.parse(u.searchParams.get('from')), to = Date.parse(u.searchParams.get('to'));
      const states = ['HEALTHY','DOWN','IDLE','UNKNOWN'];
      const durations = Object.fromEntries(states.map((s,i) => [s,(to-from)/1000*f.fractions[i]]));
      let payload;
      if (u.pathname === '/api/freshness') payload = {status:'success',data:{result:f.missing?[]:[{value:[f.now/1000,String(f.sample/1000)]}]}};
      if (u.pathname === '/api/status') payload = {repositories:f.repositories.map(repository=>({repository,current:{status:f.state,started_at:new Date(f.now-10000).toISOString(),reason:'<script>unsafe</script>'},last_observation_at:f.lastObservationAt ?? new Date(f.now).toISOString(),queue_deadline:f.queueDeadline,queue:f.queue,runtime:f.runtime?.(repository)}))};
      if (u.pathname === '/api/report') payload = {reports:f.repositories.map((repository,index)=>({repository,from:new Date(from).toISOString(),to:new Date(to).toISOString(),durations_seconds:durations,demand_availability:durations.HEALTHY+durations.DOWN ? durations.HEALTHY/(durations.HEALTHY+durations.DOWN) : null,observation_coverage:f.fractions.slice(0,3).reduce((a,b)=>a+b,0),...(f.report ? f.report(from,to,index) : {})}))};
      if (u.pathname === '/api/timeline') {
        let cursor=from;
        const rows=[];
        states.forEach((status,i) => {
          const end = i===3 ? to : cursor+(to-from)*f.fractions[i];
          if(end>cursor) rows.push({status,started_at:new Date(cursor).toISOString(),ended_at:new Date(end).toISOString()});
          cursor=end;
        });
        if (f.mismatch && rows.length) rows[0].status='UNKNOWN';
        payload={from:new Date(from).toISOString(),to:new Date(to).toISOString(),repositories:Object.fromEntries(f.repositories.map(repository=>[repository,rows]))};
      }
      return {ok:true,json:async()=>payload};
    },
  });
  vm.runInContext(code, context);
  return {f,classes,nodes,context,documentEvents,windowEvents,intervals,bars:()=>bars,refresh:()=>vm.runInContext('refresh()',context)};
}
test('transport failures, stale and missing scrape, sleep expire all displayed values and recover', async () => {
  const {f,classes,nodes,context,refresh} = fixture();
  await refresh(); assert.equal(classes.has('expired'),false);
  for (const endpoint of ['/api/freshness','/api/status','/api/report','/api/timeline']) {
    f.failure=endpoint; await refresh(); assert.equal(classes.has('expired'),true);
    f.failure=''; await refresh(); assert.equal(classes.has('expired'),false);
  }
  f.now+=45000; await refresh(); assert.equal(classes.has('expired'),true);
  f.sample=f.now; await refresh(); assert.equal(classes.has('expired'),false);
  f.missing=true; await refresh(); assert.equal(classes.has('expired'),true);
  f.missing=false; await refresh(); assert.equal(classes.has('expired'),false);
  f.now+=45000; vm.runInContext('checkExpiry()',context); assert.equal(classes.has('expired'),true);
  assert.match(nodes.error.textContent,/状態を確認できません/);
  assert.doesNotMatch(nodes.error.textContent,/UNKNOWN/);
});
test('timeline only, numeric availability, hatched gaps and coverage inside details', async () => {
  assert.match(page,/\.unobserved\{background:repeating-linear-gradient\(135deg,/);
  assert.doesNotMatch(page.split('<script>')[0].split('<body')[1],/UNKNOWN|coverage/);
  const {f,nodes,classes,refresh} = fixture();
  for (const [state,fractions] of [['HEALTHY',[1,0,0,0]],['DOWN',[0,1,0,0]],['IDLE',[0,0,1,0]],['UNKNOWN',[0,0,0,1]],['UNKNOWN',[0,0,0,0]],['DOWN',[0.25,0.25,0.25,0.25]],['HEALTHY',[0.999,0.001,0,0]]]) {
    f.state=state; f.fractions=fractions; await refresh();
    assert.equal(classes.has('expired'),false,nodes.error.textContent);
    const html=nodes.repos.innerHTML;
    if(state === 'UNKNOWN') assert.match(html,/状態を確認できません/);
    else assert.match(html,new RegExp(`<b>${state}</b>`));
    assert.match(html,/&lt;script&gt;unsafe/);
    assert.doesNotMatch(html,/<script>unsafe/);
    const normal = html.split('<details')[0];
    assert.doesNotMatch(normal,/>UNKNOWN<|title="UNKNOWN|coverage|観測率/);
    assert.equal((normal.match(/class="bar/g)||[]).length,1);
    assert.match(normal,/選択期間全体の時間に対する割合/);
    assert.doesNotMatch(page,/需要時稼働率|補助集計|class="windows"/);
    assert.doesNotMatch(normal,/<table/);
    const metrics = [...normal.matchAll(/<strong>([\d.,]+)%<\/strong>/g)].map(match=>Number(match[1]));
    assert.deepEqual(metrics,[fractions[0]*100,Math.round((fractions[0]+fractions[2])*10000)/100]);
    assert.match(html.split('<details')[1],/選択期間の観測率:.*未観測時間:/);
    const missing = fractions.slice(0,3).reduce((a,b)=>a+b,0)<1;
    assert.equal(html.includes('この期間には未観測の時間があります'),missing);
    assert.equal(html.includes('class="segment unobserved"'),missing);
    if(missing) assert.match(html,/title="観測できない区間 · .* JST → .* JST"/);
    assert.doesNotMatch(normal,/N\/A/);
    if(fractions[1]===0.001) assert.match(html,/width:0.1%/);
  }
});
test('range changes share exact bounds and keep current state independent from historical selection', async () => {
  const {f,nodes,context,refresh} = fixture();
  await refresh();
  nodes.window.value='168'; nodes.window.change(); await refresh();
  let reports=f.urls.filter(u=>u.startsWith('/api/report?'));
  const timeline=f.urls.filter(u=>u.startsWith('/api/timeline?')).at(-1);
  assert.ok(reports.includes(timeline.replace('/api/timeline','/api/report')));
  nodes.from.value='2026-01-01T00:00'; nodes.to.value='2026-01-02T00:00';
  nodes.selection.submit({preventDefault(){}}); await refresh();
  const selected=f.urls.filter(u=>u.startsWith('/api/timeline?')).at(-1);
  assert.equal(new URL(selected,'http://localhost').searchParams.get('from'),'2025-12-31T15:00:00.000Z');
  assert.match(nodes.repos.innerHTML,/<b>HEALTHY<\/b>/);
  assert.match(nodes.range.textContent,/現在カードは最新取得時点/);
  f.now+=15000; f.sample=f.now; await refresh();
  assert.equal(f.urls.filter(u=>u.startsWith('/api/timeline?')).at(-1),selected);
  assert.throws(()=>vm.runInContext(`reportDurations({from:'2026-01-01',to:'2026-01-02',durations_seconds:{}},0,1)`,context));
});

test('inconsistent replay expires and a late response cannot replace a new selection', async () => {
  const {f,nodes,classes,refresh} = fixture();
  await refresh();
  f.mismatch=true; await refresh(); assert.equal(classes.has('expired'),true);
  f.mismatch=false; await refresh(); assert.equal(classes.has('expired'),false);
  let release;
  f.pause=new Promise(resolve=>{release=resolve;});
  const old=refresh();
  f.pause=null;
  nodes.window.value='720'; nodes.window.change(); await refresh();
  const range=nodes.range.textContent;
  release(); await old;
  assert.equal(nodes.range.textContent,range);
  assert.equal(classes.has('expired'),false);
});

test('refresh preserves open and closed repository details, including recovery and selection changes', async () => {
  const {f,nodes,refresh} = fixture();
  await refresh();
  nodes.repos.querySelectorAll('details')[0].open=true;
  await refresh();
  assert.equal(nodes.repos.querySelectorAll('details')[0].open,true);
  f.failure='/api/status'; await refresh();
  f.failure=''; await refresh();
  assert.equal(nodes.repos.querySelectorAll('details')[0].open,true);
  nodes.window.value='168'; nodes.window.change(); await refresh();
  assert.equal(nodes.repos.querySelectorAll('details')[0].open,true);
  nodes.repos.querySelectorAll('details')[0].open=false;
  await refresh();
  assert.equal(nodes.repos.querySelectorAll('details')[0].open,false);
});

test('JST dates, details and custom bounds are independent of the browser timezone', async () => {
  const previousTZ = process.env.TZ;
  try {
    for (const timezone of ['UTC','America/Los_Angeles','Asia/Tokyo']) {
      process.env.TZ = timezone;
      const {f,nodes,context,classes,refresh} = fixture();
      f.now = f.sample = Date.parse('2026-09-06T15:00:00Z');
      f.queueDeadline = '2026-09-06T15:30:00Z';
      f.queue = [{number:1,phase:'ready',deadline:'2026-09-06T16:00:00Z'}];
      await refresh();
      assert.equal(classes.has('expired'),false);
      assert.equal(vm.runInContext("stamp('2026-12-31T15:00:00Z')",context),'2027-01-01 00:00:00 JST');
      assert.match(nodes.freshness.textContent,/取得 09-07 00:00 JST/);
      assert.match(nodes.range.textContent,/2026-09-06 00:00:00 JST → 2026-09-07 00:00:00 JST/);
      const html = nodes.repos.innerHTML;
      assert.match(html,/状態開始: 2026-09-06 23:59:50 JST/);
      assert.match(html,/最終観測: 2026-09-07 00:00:00 JST/);
      assert.match(html,/queue期限: 2026-09-07 00:30:00 JST/);
      assert.match(html,/個別参考期限（全体判定には不使用） 2026-09-07 01:00:00 JST/);
      assert.match(html,/title="正常 · 2026-09-06 00:00:00 JST → 2026-09-06 06:00:00 JST"/);
      assert.match(html,/<span>09-06 00:00 JST<\/span><span>09-07 00:00 JST<\/span>/);
      nodes.window.value='custom'; nodes.window.change();
      nodes.from.value='2026-09-06T23:30'; nodes.to.value='2026-09-07T00:00';
      nodes.selection.submit({preventDefault(){}}); await refresh();
      const timeline = f.urls.filter(u=>u.startsWith('/api/timeline?')).at(-1);
      const query = new URL(timeline,'http://localhost').searchParams;
      assert.equal(query.get('from'),'2026-09-06T14:30:00.000Z');
      assert.equal(query.get('to'),'2026-09-06T15:00:00.000Z');
      assert.ok(f.urls.includes(timeline.replace('/api/timeline','/api/report')));
      assert.equal(classes.has('expired'),false);
      f.state = 'UNKNOWN'; f.queueDeadline = '0001-01-01T00:00:00Z';
      await refresh();
      assert.match(nodes.repos.innerHTML,/queue期限: —/);
      assert.doesNotMatch(nodes.repos.innerHTML,/0001-01-01/);
    }
  } finally {
    if (previousTZ === undefined) delete process.env.TZ;
    else process.env.TZ = previousTZ;
  }
});

test('one hour selection sends exactly 3600 seconds and retains the default without auxiliary requests', async () => {
  assert.match(page,/<option value="1">直近1h<\/option>/);
  assert.match(page,/<option value="24" selected>直近24h<\/option>/);
  assert.match(page,/開始（JST）/);
  assert.match(page,/終了（JST）/);
  const {f,nodes,classes,refresh} = fixture();
  await refresh();
  let timeline = f.urls.filter(u=>u.startsWith('/api/timeline?')).at(-1);
  let query = new URL(timeline,'http://localhost').searchParams;
  assert.equal(Date.parse(query.get('to'))-Date.parse(query.get('from')),86400000);
  nodes.window.value='1'; nodes.window.change();
  f.urls=[];
  await refresh();
  timeline = f.urls.find(u=>u.startsWith('/api/timeline?'));
  query = new URL(timeline,'http://localhost').searchParams;
  assert.equal(Date.parse(query.get('to'))-Date.parse(query.get('from')),3600000);
  assert.ok(f.urls.includes(timeline.replace('/api/timeline','/api/report')));
  const reports = f.urls.filter(u=>u.startsWith('/api/report?')).map(url=> {
    const params = new URL(url,'http://localhost').searchParams;
    assert.equal(params.get('to'),query.get('to'));
    return (Date.parse(params.get('to'))-Date.parse(params.get('from')))/1000;
  });
  assert.deepEqual(reports,[3600]);
  assert.equal(classes.has('expired'),false);
  assert.match(nodes.repos.innerHTML,/api\/timeline\?repo=owner%2Frepo&amp;from=/);
});


test('historical 100 second metrics use state durations and include unknown and unobserved time in the denominator', async () => {
  const {f,nodes,classes,refresh} = fixture();
  await refresh();
  f.repositories=['ishii1648/codex-issue-loop','ishii1648/zeitreise'];
  f.fractions=[0.4,0.2,0.3,0.1];
  nodes.from.value='2026-01-01T00:00:00'; nodes.to.value='2026-01-01T00:01:40';
  nodes.selection.submit({preventDefault(){}});
  for (const unknown of [10,0]) {
    f.report=()=>({durations_seconds:{HEALTHY:40,DOWN:20,IDLE:30,UNKNOWN:unknown},demand_availability:null});
    f.now+=15000; f.sample=f.now; f.urls=[];
    await refresh();
    assert.equal(classes.has('expired'),false,nodes.error.textContent);
    const reports=f.urls.filter(u=>u.startsWith('/api/report?')).map(url=>new URL(url,'http://localhost').searchParams);
    assert.equal(reports.length,1);
    assert.equal(reports[0].get('from'),'2025-12-31T15:00:00.000Z');
    assert.equal(reports[0].get('to'),'2025-12-31T15:01:40.000Z');
    const articles=nodes.repos.innerHTML.split('<article>').slice(1);
    assert.equal(articles.length,2);
    articles.forEach((html,index)=>{
      assert.ok(html.startsWith(`<div class="repo-heading"><h2>${f.repositories[index]}</h2>`));
      assert.match(html,/稼働率（正常のみ）<strong>40%<\/strong>/);
      assert.match(html,/正常動作率（正常\+待機）<strong>70%<\/strong>/);
      assert.match(html,/未観測時間: 10秒/);
      assert.doesNotMatch(html,/<table|需要時稼働率|N\/A/);
    });
  }
  for (const boundary of ['from','to']) {
    f.report=(from,to,index)=>index===1 ? {[boundary]:new Date((boundary==='from' ? from : to)+1000).toISOString()} : {};
    await refresh();
    assert.equal(classes.has('expired'),true);
    assert.match(nodes.error.textContent,/期間集計が不一致/);
  }
});

test('unset and zero dates display dashes for IDLE snapshots and unproven queue items', async () => {
  const {f,nodes,classes,refresh} = fixture();
  f.state='IDLE';
  for (const value of [undefined,'0001-01-01T00:00:00Z','invalid']) {
    f.queueDeadline=value;
    f.lastObservationAt=value === undefined ? '' : value;
    f.queue=[];
    await refresh();
    assert.equal(classes.has('expired'),false);
    assert.match(nodes.repos.innerHTML,/queue期限: —/);
    assert.match(nodes.repos.innerHTML,/最終観測: —/);
    f.state='UNKNOWN';
    f.queue=[{number:1,phase:'ready',deadline:value}];
    await refresh();
    assert.equal(classes.has('expired'),false);
    assert.match(nodes.repos.innerHTML,/個別参考期限（全体判定には不使用） —/);
    assert.doesNotMatch(nodes.repos.innerHTML,/0001-/);
    f.state='IDLE';
  }
});

function pointer(nodes,bar,type,x,extra={}) {
  nodes.repos[type]({target:bar,clientX:x,pointerId:1,isPrimary:true,button:0,preventDefault(){},...extra});
}

test('drag arbitrary positions in either direction, clamp bounds and apply exact JST inputs to all repositories', async () => {
  for (const [start,end] of [[423.456,876.543],[876.543,423.456],[600,1500],[600,-100],[880,1030]]) {
    const {f,nodes,bars,refresh} = fixture();
    f.repositories=['owner/one','owner/two'];
    await refresh();
    const bar=bars()[1];
    const baseFrom=Number(bar.dataset.from), baseTo=Number(bar.dataset.to);
    pointer(nodes,bar,'pointerdown',start);
    pointer(nodes,bar,'pointermove',end);
    assert.equal(bar.overlay.hidden,false);
    assert.ok(parseFloat(bar.overlay.style.width)>0);
    pointer(nodes,bar,'pointerup',end);
    assert.equal(bar.capture,null);
    assert.equal(bar.overlay.removed,true);
    assert.equal(nodes.window.value,'custom');
    const from=Math.floor(baseFrom+(Math.max(100,Math.min(start,end))-100)/1000*(baseTo-baseFrom));
    const to=Math.ceil(baseFrom+(Math.min(1100,Math.max(start,end))-100)/1000*(baseTo-baseFrom));
    assert.equal(Date.parse(nodes.from.value+'+09:00'),from);
    assert.equal(Date.parse(nodes.to.value+'+09:00'),to);
    await refresh();
    const url=f.urls.filter(u=>u.startsWith('/api/timeline?')).at(-1);
    const query=new URL(url,'http://localhost').searchParams;
    assert.equal(Date.parse(query.get('from')),from);
    assert.equal(Date.parse(query.get('to')),to);
    assert.ok(f.urls.includes(url.replace('/api/timeline','/api/report')));
    assert.equal(bars().length,2);
    for (const rendered of bars()) assert.deepEqual(rendered.dataset,{from:String(from),to:String(to)});
    assert.equal((nodes.repos.innerHTML.match(/<b>HEALTHY<\/b>/g)||[]).length,2);
    nodes.selection.submit({preventDefault(){}}); await refresh();
    assert.equal(f.urls.filter(u=>u.startsWith('/api/timeline?')).at(-1),url);
    nodes.window.value='24'; nodes.window.change(); await refresh();
    assert.equal(Number(bars()[0].dataset.to)-Number(bars()[0].dataset.from),86400000);
  }
});

test('click, small movement, pointer cancellation, lost capture, Escape and blur release selection without applying', async () => {
  for (const cancel of ['click','small','pointercancel','lostpointercapture','Escape','blur','hidden']) {
    const {f,nodes,bars,context,documentEvents,windowEvents,refresh} = fixture();
    await refresh();
    const bar=bars()[0], requests=f.urls.length;
    pointer(nodes,bar,'pointerdown',500);
    pointer(nodes,bar,'pointermove',cancel==='small'?504:700);
    if(cancel==='click' || cancel==='small') pointer(nodes,bar,'pointerup',cancel==='small'?504:500);
    else if(cancel==='Escape') documentEvents.keydown({key:'Escape'});
    else if(cancel==='blur') windowEvents.blur();
    else if(cancel==='hidden') { vm.runInContext('document.hidden=true',context); documentEvents.visibilitychange(); }
    else pointer(nodes,bar,cancel,700);
    assert.equal(bar.overlay.removed,true,cancel);
    assert.equal(bar.capture,null,cancel);
    assert.equal(vm.runInContext('drag',context),null);
    assert.equal(vm.runInContext('selected.hours',context),24);
    assert.equal(f.urls.length,requests);
  }
});

test('drag retains rendered bounds across in-flight refresh and automatic updates while expiry still cancels', async () => {
  const {f,nodes,bars,context,classes,refresh} = fixture();
  await refresh();
  const bar=bars()[0], baseFrom=Number(bar.dataset.from), baseTo=Number(bar.dataset.to);
  let release;
  f.now+=15000; f.sample=f.now;
  f.pause=new Promise(resolve=>{release=resolve;});
  const old=refresh(); f.pause=null;
  pointer(nodes,bar,'pointerdown',350);
  const requests=f.urls.length;
  await refresh(); assert.equal(f.urls.length,requests);
  release(); await old;
  assert.equal(bars()[0],bar);
  pointer(nodes,bar,'pointerup',850);
  await refresh();
  assert.equal(Number(bars()[0].dataset.from),baseFrom+(baseTo-baseFrom)*0.25);
  assert.equal(Number(bars()[0].dataset.to),baseFrom+(baseTo-baseFrom)*0.75);
  const current=bars()[0];
  pointer(nodes,current,'pointerdown',350);
  f.now+=45000;
  vm.runInContext('checkExpiry()',context);
  assert.equal(classes.has('expired'),true);
  assert.equal(current.overlay.removed,true);
  assert.equal(vm.runInContext('drag',context),null);
  pointer(nodes,current,'pointerup',850);
  assert.equal(Number(bars()[0].dataset.from),baseFrom+(baseTo-baseFrom)*0.25);
});

test('narrow sub-second range stays nonempty at input precision', async () => {
  const {nodes,bars,refresh} = fixture();
  nodes.from.value='2026-01-01T00:00:00.001'; nodes.to.value='2026-01-01T00:00:00.005';
  nodes.selection.submit({preventDefault(){}}); await refresh();
  const bar=bars()[0];
  pointer(nodes,bar,'pointerdown',500);
  pointer(nodes,bar,'pointerup',506);
  assert.ok(Date.parse(nodes.from.value+'+09:00')<Date.parse(nodes.to.value+'+09:00'));
  assert.match(page,/type="datetime-local" step="0.001"/);
});


test('runtime versions map to repositories, update independently of range, escape text and clear on missing data', async () => {
  const {f,nodes,classes,refresh} = fixture();
  f.repositories=['owner/one','owner/two'];
  let versions={'owner/one':'1.8.2','owner/two':'v1.8.1'};
  f.runtime=repo=>versions[repo] ? {version:versions[repo],observed_at:new Date(f.now).toISOString(),expires_at:new Date(f.now+180000).toISOString()} : null;
  await refresh();
  const articles=nodes.repos.innerHTML.split('<article>').slice(1);
  assert.match(articles[0],/Runtime <code>v1.8.2<\/code>/);
  assert.match(articles[1],/Runtime <code>v1.8.1<\/code>/);
  nodes.from.value='2026-01-01T00:00'; nodes.to.value='2026-01-02T00:00';
  nodes.selection.submit({preventDefault(){}}); await refresh();
  versions['owner/one']='1.9.0'; f.now+=15000; f.sample=f.now;
  await refresh();
  assert.match(nodes.repos.innerHTML,/Runtime <code>v1.9.0<\/code>/);
  assert.ok(f.urls.filter(url=>url.startsWith('/api/status')).every(url=>url==='/api/status'));
  delete versions['owner/one']; await refresh();
  assert.match(nodes.repos.innerHTML.split('<article>')[1],/Runtime 不明/);
  assert.match(nodes.repos.innerHTML.split('<article>')[2],/Runtime <code>v1.8.1/);
  versions['owner/one']='<script>bad</script>'; await refresh();
  assert.match(nodes.repos.innerHTML,/v&lt;script&gt;bad/);
  assert.equal(classes.has('expired'),false);
});

test('runtime timestamps never override a valid version while overall freshness remains enforced', async () => {
  const {f,nodes,context,classes,refresh} = fixture();
  for (const offset of [-180000,1800,180000]) {
    f.runtime=()=>({version:'1.8.2',observed_at:new Date(f.now+offset).toISOString(),expires_at:new Date(f.now+offset+1000).toISOString()});
    await refresh();
    f.now+=5000;
    vm.runInContext('checkExpiry()',context);
    assert.match(nodes.repos.querySelectorAll('.runtime-badge')[0].textContent,/v1.8.2/);
    assert.equal(classes.has('expired'),false);
  }
  for (const runtime of [{version:'1.8.2'}, {version:'1.8.2',observed_at:'bad',expires_at:'bad'}]) {
    f.runtime=()=>runtime; await refresh();
    assert.match(nodes.repos.innerHTML,/Runtime <code>v1.8.2/);
  }
  for (const runtime of [null,undefined,{}, {version:''}, {version:'  '}, {version:123}, {version:{}}]) {
    f.runtime=()=>({version:'1.8.2'}); await refresh();
    f.runtime=()=>runtime; await refresh();
    assert.match(nodes.repos.innerHTML,/Runtime 不明/);
    assert.doesNotMatch(nodes.repos.innerHTML,/v1.8.2/);
    assert.match(nodes.repos.innerHTML,/<b>HEALTHY<\/b>/);
    assert.equal(classes.has('expired'),false);
  }
});

test('initial load, interval, tab return and page restoration fetch current runtime and recover after sleep or failure', async () => {
  const {f,nodes,context,classes,intervals,documentEvents,windowEvents} = fixture({runtime:()=>({version:'1.8.2'})});
  const settle = () => new Promise(resolve=>setImmediate(resolve));
  const statusRequests = () => f.urls.filter(url=>url==='/api/status').length;
  await settle();
  assert.equal(statusRequests(),1);
  assert.match(nodes.repos.innerHTML,/Runtime <code>v1.8.2/);
  assert.deepEqual([...intervals.keys()],[1000,15000]);
  f.runtime=()=>({version:'1.9.0'});
  f.now+=15000; f.sample=f.now;
  await intervals.get(15000)();
  assert.equal(statusRequests(),2);
  assert.match(nodes.repos.innerHTML,/Runtime <code>v1.9.0/);
  vm.runInContext('document.hidden=true',context); documentEvents.visibilitychange();
  assert.equal(statusRequests(),2);
  f.runtime=()=>null;
  vm.runInContext('document.hidden=false',context); documentEvents.visibilitychange(); await settle();
  assert.equal(statusRequests(),3);
  assert.match(nodes.repos.innerHTML,/Runtime 不明/);
  f.runtime=()=>({version:'2.0.0'});
  windowEvents.pageshow({persisted:true}); await settle();
  assert.equal(statusRequests(),4);
  assert.match(nodes.repos.innerHTML,/Runtime <code>v2.0.0/);
  f.now+=60000;
  intervals.get(1000)();
  assert.equal(classes.has('expired'),true);
  f.sample=f.now;
  f.runtime=()=>({version:'2.1.0'});
  await intervals.get(15000)();
  assert.equal(classes.has('expired'),false);
  assert.match(nodes.repos.innerHTML,/Runtime <code>v2.1.0/);
  f.failure='/api/status';
  await intervals.get(15000)();
  assert.equal(classes.has('expired'),true);
  f.failure=''; f.runtime=()=>null;
  await intervals.get(15000)();
  assert.equal(classes.has('expired'),false);
  assert.match(nodes.repos.innerHTML,/Runtime 不明/);
  assert.doesNotMatch(nodes.repos.innerHTML,/v2.1.0/);
});
