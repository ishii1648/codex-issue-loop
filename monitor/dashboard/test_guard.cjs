const {readFileSync} = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const {test} = require('node:test');
const page = readFileSync(__dirname + '/../internal/app/dashboard.html', 'utf8');
const code = page.split('<script>')[1].split('</script>')[0];
function fixture() {
  const f = {now:1800000000000, sample:1800000000000, failure:'', missing:false, state:'HEALTHY', repositories:['owner/repo'], fractions:[0.25,0.25,0.25,0.25], urls:[]};
  const classes = new Set(['expired']);
  const nodes = Object.fromEntries(['freshness','error','repos','range','window','custom','selection','from','to'].map(id => [id,{addEventListener(name,fn){this[name]=fn;},classList:{toggle(){}}}]));
  let details = [];
  let html = '';
  Object.defineProperty(nodes.repos, 'innerHTML', {
    get:()=>html,
    set:value=>{ html=value; details=[...value.matchAll(/<details data-repository="([^"]+)"/g)].map(match=>({dataset:{repository:match[1]},open:false})); },
  });
  nodes.repos.querySelectorAll = selector => details.filter(detail => selector !== 'details[open]' || detail.open);
  const context = vm.createContext({
    document:{body:{classList:{add:x=>classes.add(x),remove:x=>classes.delete(x)}},getElementById:x=>nodes[x],addEventListener(){}},
    window:{addEventListener(){}},Date:class extends Date {static now(){return f.now;}},
    performance:{now:()=>f.now},setInterval(){},AbortSignal,URLSearchParams,
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
      if (u.pathname === '/api/status') payload = {repositories:f.repositories.map(repository=>({repository,current:{status:f.state,started_at:new Date(f.now-10000).toISOString(),reason:'<script>unsafe</script>'},last_observation_at:new Date(f.now).toISOString(),queue_deadline:f.queueDeadline,queue:f.queue}))};
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
  return {f,classes,nodes,context,refresh:()=>vm.runInContext('refresh()',context)};
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
    assert.doesNotMatch(normal,/>UNKNOWN<|title="UNKNOWN|coverage|観測率|選択期間全体/);
    assert.equal((normal.match(/class="bar/g)||[]).length,1);
    assert.match(normal,/観測できた需要時間に対する割合/);
    assert.match(html.split('<details')[1],/選択期間の観測率:.*未観測時間:/);
    const missing = fractions.slice(0,3).reduce((a,b)=>a+b,0)<1;
    assert.equal(html.includes('この期間には未観測の時間があります'),missing);
    assert.equal(html.includes('class="segment unobserved"'),missing);
    if(missing) assert.match(html,/title="観測できない区間 · .* JST → .* JST"/);
    if(fractions[0]+fractions[1]===0) assert.match(html,/N\/A/);
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
      assert.match(html,/期限 2026-09-07 01:00:00 JST/);
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
    }
  } finally {
    if (previousTZ === undefined) delete process.env.TZ;
    else process.env.TZ = previousTZ;
  }
});

test('one hour selection sends exactly 3600 seconds and retains default and auxiliary windows', async () => {
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
  assert.deepEqual(reports,[3600,3600,86400,604800,2592000]);
  assert.equal(classes.has('expired'),false);
  assert.match(nodes.repos.innerHTML,/api\/timeline\?repo=owner%2Frepo&amp;from=/);
});


test('auxiliary windows keep exact current bounds and ordered repository values for historical selection', async () => {
  const {f,nodes,classes,refresh} = fixture();
  await refresh();
  f.repositories=['ishii1648/codex-issue-loop','ishii1648/zeitreise'];
  const seconds=[3600,86400,604800,2592000];
  const availability=[[null,0.24,0.7,0.3],[0.1,0.48,0.77,1]];
  f.report=(from,to,index)=>to===f.now ? {demand_availability:availability[index][seconds.indexOf((to-from)/1000)]} : {};
  nodes.from.value='2026-01-01T00:00'; nodes.to.value='2026-01-02T00:00';
  nodes.selection.submit({preventDefault(){}});
  for (let update=0;update<2;update++) {
    f.now+=15000; f.sample=f.now; f.urls=[];
    await refresh();
    assert.equal(classes.has('expired'),false,nodes.error.textContent);
    const reports=f.urls.filter(u=>u.startsWith('/api/report?')).map(url=>new URL(url,'http://localhost').searchParams);
    assert.equal(reports.length,5);
    assert.equal(reports[0].get('to'),'2026-01-01T15:00:00.000Z');
    assert.deepEqual(reports.slice(1).map(params=>{
      assert.equal(Date.parse(params.get('to')),f.now);
      return (Date.parse(params.get('to'))-Date.parse(params.get('from')))/1000;
    }),seconds);
    const articles=nodes.repos.innerHTML.split('<article>').slice(1);
    assert.equal(articles.length,2);
    articles.forEach((html,index)=>{
      assert.ok(html.startsWith(`<h2>${f.repositories[index]}</h2>`));
      const rows=[...html.matchAll(/<tr><th>([^<]+)<\/th><td>([^<]+)<\/td><\/tr>/g)].map(match=>match.slice(1));
      assert.deepEqual(rows,index===0 ? [['1h','N/A'],['24h','24%'],['7d','70%'],['30d','30%']] : [['1h','10%'],['24h','48%'],['7d','77%'],['30d','100%']]);
    });
  }
  for (const duration of seconds) {
    for (const boundary of ['from','to']) {
      f.report=(from,to,index)=>index===1 && to===f.now && (to-from)/1000===duration ? {[boundary]:new Date((boundary==='from' ? from : to)+1000).toISOString()} : {};
      await refresh();
      assert.equal(classes.has('expired'),true);
      assert.match(nodes.error.textContent,/期間集計が不一致/);
    }
  }
});
