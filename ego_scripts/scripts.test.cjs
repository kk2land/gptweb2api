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
