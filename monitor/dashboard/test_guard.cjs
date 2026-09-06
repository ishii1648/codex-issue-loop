const {readFileSync} = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const {test} = require('node:test');
const code = readFileSync(__dirname + '/../internal/app/dashboard.html', 'utf8').split('<script>')[1].split('</script>')[0];
test('HEALTHY frame is hidden on transport failure, stale samples, missing scrape and sleep, then recovers', async () => {
  let now = 1800000000000;
  let sample = now;
  let failure = false;
  let missing = false;
  const classes = new Set(['expired']);
  const nodes = {freshness:{},error:{}};
  const context = vm.createContext({
    document:{body:{classList:{add:x=>classes.add(x),remove:x=>classes.delete(x)}},getElementById:x=>nodes[x],addEventListener(){}},
    window:{addEventListener(){}},
    Date:class extends Date {static now(){return now;}},
    performance:{now:()=>now},setInterval(){},AbortSignal,
    fetch:async()=> {if(failure) throw new Error('offline');return {ok:true,json:async()=>({status:'success',data:{result:missing?[]:[{value:[now/1000,String(sample/1000)]}]}})};},
  });
  vm.runInContext(code, context);
  const refresh = () => vm.runInContext('refresh()', context);
  await refresh(); assert.equal(classes.has('expired'),false);
  failure=true; await refresh(); assert.equal(classes.has('expired'),true);
  failure=false; await refresh(); assert.equal(classes.has('expired'),false);
  now+=45000; await refresh(); assert.equal(classes.has('expired'),true);
  sample=now; await refresh(); assert.equal(classes.has('expired'),false);
  missing=true; await refresh(); assert.equal(classes.has('expired'),true);
  missing=false; await refresh(); assert.equal(classes.has('expired'),false);
  now+=45000; vm.runInContext('checkExpiry()',context); assert.equal(classes.has('expired'),true);
  assert.match(nodes.error.textContent,/UNKNOWN/);
});
