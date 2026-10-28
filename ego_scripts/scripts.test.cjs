const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const common = fs.readFileSync(path.join(__dirname,'common.js'),'utf8');
function context() { const ctx = vm.createContext({setTimeout,clearTimeout}); vm.runInContext(common,ctx); return ctx; }

test('every external script parses independently with the frozen common code',()=>{
  for(const name of fs.readdirSync(__dirname).filter(n=>n.endsWith('.js')&&n!=='common.js')){
    new vm.Script(`${common}\n${fs.readFileSync(path.join(__dirname,name),'utf8')}`,{filename:name});
  }
});
test('requested model never silently maps to a different version',()=>{
  const ctx=context();assert.equal(vm.runInContext(`modelMatches('ChatGPT GPT-6','web/gpt-6')`,ctx),true);
  assert.equal(vm.runInContext(`modelMatches('GPT-5.6 Sol','web/gpt-5.6-sol')`,ctx),true);
  for(const label of ['GPT-5','GPT-6 mini','GPT-6 Thinking']){ctx.label=label;assert.equal(vm.runInContext(`modelMatches(label,'web/gpt-6')`,ctx),false);}
});
test('preflight rejects missing plugin evidence and mismatched page identity',()=>{
  const ctx=context();ctx.state={host:'chatgpt.com',input:true,conversation:'conversation',project:'project',model:'GPT-6',plugins:[{id:'actual-plugin',selected:true}],last_markers:['req_expected']};
  ctx.input={conversation_id:'conversation',project_id:'project',model:'web/gpt-6',plugin_id:'actual-plugin',expected_last_request_id:'req_expected'};
  assert.doesNotThrow(()=>vm.runInContext('verify(state,input)',ctx));
  for(const [key,value,code] of [['conversation','different','conversation_mismatch'],['project','different','project_mismatch'],['plugins',[],'plugin_identity_unverified'],['last_markers',[],'page_history_mismatch']]){
    const old=ctx.state[key];ctx.state[key]=value;assert.throws(()=>vm.runInContext('verify(state,input)',ctx),e=>e.code===code);ctx.state[key]=old;
  }
  ctx.input.plugin_id='other-plugin';assert.throws(()=>vm.runInContext('verify(state,input)',ctx),e=>e.code==='plugin_identity_mismatch');
});

function spaceContext(initial) {
  const ctx=context();let spaces=initial.map(s=>({...s}));const calls={created:[],claimed:[]};
  ctx.ego={
    listTaskSpaces:async()=>({taskSpaces:spaces.map(s=>({...s}))}),
    createTaskSpace:async(...args)=>{calls.created.push(args);const id=100+calls.created.length;spaces.push({id,name:args[0],profileId:args[1]||'profile-1',ownership:'agent'});return {id};},
    claimTaskSpace:async(...args)=>{calls.claimed.push(args);spaces=spaces.map(s=>s.id===args[0]?{...s,ownership:'agent'}:s);return {id:args[0]};}
  };
  return {ctx,calls};
}
function selectSpace(ctx,input) { ctx.input=input;return vm.runInContext('ensureTaskSpace(input)',ctx); }
test('default naming does not claim a colliding Space or pass undefined native arguments',async()=>{
  const {ctx,calls}=spaceContext([{id:7,name:'gptweb2api-slot-0',profileId:'profile-1',ownership:'user'}]);
  await assert.rejects(selectSpace(ctx,{space_name:'gptweb2api-slot-0'}),e=>e.code==='space_requires_reconciliation');
  assert.equal(calls.claimed.length,0);
  await selectSpace(ctx,{space_name:'gptweb2api-slot-1'});
  assert.deepEqual(calls.created,[['gptweb2api-slot-1']]);
});
test('bound ID survives display-name changes and never reclaims user control',async()=>{
  const {ctx,calls}=spaceContext([{id:7,name:'renamed',profileId:'profile-1',ownership:'agent'}]);
  const b={space_id:7,profile_id:'profile-1',page_id:'gateway-tab'};
  assert.equal((await selectSpace(ctx,{space_name:'original',binding:b})).space_id,7);
  const controlled=spaceContext([{id:7,name:'renamed',profileId:'profile-1',ownership:'user'}]);
  await assert.rejects(selectSpace(controlled.ctx,{space_name:'original',binding:b}),e=>e.code==='space_binding_changed');
  const delegated=spaceContext([{id:7,name:'original',profileId:'profile-1',ownership:'agentDelegatedToUser'}]);
  await assert.rejects(selectSpace(delegated.ctx,{space_name:'original',binding:b}),e=>e.code==='space_binding_changed');
  assert.equal(calls.claimed.length,0);assert.equal(controlled.calls.claimed.length,0);assert.equal(delegated.calls.claimed.length,0);
});
