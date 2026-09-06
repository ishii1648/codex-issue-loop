const {readFileSync} = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const {test} = require('node:test');
const code = readFileSync(__dirname + '/../internal/app/dashboard.html', 'utf8').split('<script>')[1].split('</script>')[0];
function fixture() {
  const f = {now:1800000000000, sample:1800000000000, failure:'', missing:false, state:'HEALTHY', fractions:[0.25,0.25,0.25,0.25], urls:[]};
  const classes = new Set(['expired']);
  const nodes = Object.fromEntries(['freshness','error','repos','range','window','custom','selection','from','to'].map(id => [id,{addEventListener(name,fn){this[name]=fn;},classList:{toggle(){}}}]));
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
      if (u.pathname === '/api/status') payload = {repositories:[{repository:'owner/repo',current:{status:f.state,started_at:new Date(f.now-10000).toISOString(),reason:'<script>unsafe</script>'},last_observation_at:new Date(f.now).toISOString()}]};
      if (u.pathname === '/api/report') payload = {reports:[{repository:'owner/repo',from:new Date(from).toISOString(),to:new Date(to).toISOString(),durations_seconds:durations,demand_availability:durations.HEALTHY+durations.DOWN ? durations.HEALTHY/(durations.HEALTHY+durations.DOWN) : null,observation_coverage:f.fractions.slice(0,3).reduce((a,b)=>a+b,0)}]};
      if (u.pathname === '/api/timeline') {
        let cursor=from;
        const rows=[];
        states.forEach((status,i) => {
          const end = i===3 ? to : cursor+(to-from)*f.fractions[i];
          if(end>cursor) rows.push({status,started_at:new Date(cursor).toISOString(),ended_at:new Date(end).toISOString()});
          cursor=end;
        });
        if (f.mismatch && rows.length) rows[0].status='UNKNOWN';
        payload={from:new Date(from).toISOString(),to:new Date(to).toISOString(),repositories:{'owner/repo':rows}};
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
  assert.match(nodes.error.textContent,/UNKNOWN/);
});
test('four-state time summary, no demand, gaps, tiny slices and replay use report and exact timeline', async () => {
  const {f,nodes,classes,refresh} = fixture();
  for (const [state,fractions] of [['HEALTHY',[1,0,0,0]],['DOWN',[0,1,0,0]],['IDLE',[0,0,1,0]],['UNKNOWN',[0,0,0,1]],['UNKNOWN',[0,0,0,0]],['DOWN',[0.25,0.25,0.25,0.25]],['HEALTHY',[0.999,0.001,0,0]]]) {
    f.state=state; f.fractions=fractions; await refresh();
    assert.equal(classes.has('expired'),false,nodes.error.textContent);
    const html=nodes.repos.innerHTML;
    assert.match(html,new RegExp(`<b>${state}</b>`));
    assert.match(html,/&lt;script&gt;unsafe/);
    assert.doesNotMatch(html,/<script>unsafe/);
    assert.equal(html.includes('期間中DOWNあり'),fractions[1]>0);
    assert.equal(html.includes('観測不足（UNKNOWN'),fractions.slice(0,3).reduce((a,b)=>a+b,0)<1);
    if(fractions[0]+fractions[1]===0) assert.match(html,/N\/A/);
    if(fractions[1]===0.001) assert.match(html,/86.4秒/);
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
  assert.equal(new URL(selected,'http://localhost').searchParams.get('from'),new Date(nodes.from.value).toISOString());
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
