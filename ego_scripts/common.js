// Native EgoBindings contract: citrolabs/ego-lite docs/native-bindings-api.md.
// Loaded once per execution attempt. No external modules or private HTTP APIs.
function fail(code) { const e = new Error(code); e.code = code; throw e; }
function checked(value) { if (value?.error) fail(value.error_code || 'ego_operation_failed'); return value; }
async function ensureTaskSpace(input) {
  if (!globalThis.ego) fail('ego_bindings_missing');
  let b={...(input.binding||{})};
  let spaces=checked(await ego.listTaskSpaces()).taskSpaces;
  if(!Array.isArray(spaces))fail('space_list_unavailable');
  if(b.space_id){
    const space=spaces.find(s=>s.id===b.space_id);
    if(!space||space.ownership!=='agent'||space.profileId!==b.profile_id||(input.profile_id&&space.profileId!==input.profile_id))fail('space_binding_changed');
    return b;
  }
  if(typeof input.space_name!=='string'||!input.space_name.trim())fail('space_name_required');
  if(spaces.some(s=>s.name===input.space_name))fail('space_requires_reconciliation');
  const created=checked(input.profile_id?await ego.createTaskSpace(input.space_name,input.profile_id):await ego.createTaskSpace(input.space_name));
  const id=created.id;
  spaces=checked(await ego.listTaskSpaces()).taskSpaces;
  const space=spaces.find(s=>s.id===id);
  if(!Number.isSafeInteger(id)||id<=0||!space?.profileId||space.ownership!=='agent')fail('space_identity_unverified');
  if(input.profile_id&&space.profileId!==input.profile_id)fail('profile_mismatch');
  return {space_id:id,profile_id:space.profileId,page_id:'',conversation_id:''};
}
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
let nextCDPID = 1;
const cdpPending = new Map();
function initCDP() {
  if (!globalThis.ego) fail('ego_bindings_missing');
  ego.onCDPMessage = raw => {
    const msg = JSON.parse(raw), pending = cdpPending.get(msg.id);
    if (!pending) return;
    cdpPending.delete(msg.id);
    clearTimeout(pending.timer);
    msg.error ? pending.reject(Object.assign(new Error('cdp_failed'), {code:'ego_cdp_failed'})) : pending.resolve(msg.result);
  };
  ego.onSendCDPMessageError = () => {
    for (const p of cdpPending.values()) { clearTimeout(p.timer); p.reject(Object.assign(new Error('cdp_failed'), {code:'ego_cdp_failed'})); }
    cdpPending.clear();
  };
}
function cdp(method, params = {}, sessionId) {
  return new Promise((resolve, reject) => {
    const id = nextCDPID++;
    const timer = setTimeout(() => { cdpPending.delete(id); reject(Object.assign(new Error('cdp_timeout'), {code:'ego_cdp_timeout'})); }, 12000);
    cdpPending.set(id, {resolve, reject, timer});
    ego.sendCDPMessage(JSON.stringify({id,method,params,...(sessionId ? {sessionId} : {})}));
  });
}
async function connect(input) {
  initCDP();
  const b = input.binding;
  if (!b?.space_id || !b.page_id) fail('page_binding_missing');
  ego.useTaskSpace(b.space_id);
  const spaces = checked(await ego.listTaskSpaces()).taskSpaces;
  const space = spaces.find(s => s.id === b.space_id);
  if (!space || space.ownership !== 'agent' || (b.profile_id && space.profileId !== b.profile_id)) fail('space_binding_changed');
  const tabs = checked(await ego.listTabs()).tabs;
  if (!tabs.some(t => t.targetId === b.page_id)) fail('page_binding_changed');
  const attached = await cdp('Target.attachToTarget', {targetId:b.page_id,flatten:true});
  return {binding:b,session:attached.sessionId};
}
async function evaluate(page, fn, ...args) {
  // JSON serialization keeps arguments separate from the prewritten function.
  const result = await cdp('Runtime.evaluate', {expression:`(${fn.toString()})(...${JSON.stringify(args)})`,awaitPromise:true,returnByValue:true}, page.session);
  if (result.exceptionDetails) fail('page_evaluation_failed');
  return result.result?.value;
}
async function observe(page) {
  return evaluate(page, () => {
    const visible = el => !!el && el.getClientRects().length > 0;
    const text = el => (el?.textContent || '').trim();
    const url = new URL(location.href);
    const conversation = url.pathname.match(/\/c\/([^/]+)/)?.[1] || '';
    const project = url.pathname.match(/^\/g\/(g-p-[^/]+)/)?.[1] || '';
    const input = document.querySelector('#prompt-textarea');
    const model = document.querySelector('[data-testid="model-switcher-dropdown-button"]');
    const stop = document.querySelector('[data-testid="stop-button"],button[aria-label="Stop generating"],button[aria-label="停止生成"]');
    const approvals = [...document.querySelectorAll('[role="dialog"] button')].some(el => visible(el) && /^(Allow|Approve|Confirm|允许|批准|确认)$/.test(text(el)));
    const plugins = [...document.querySelectorAll('[data-plugin-id],[data-app-id]')].filter(el => visible(el) && /gptweb2api/i.test(text(el) + ' ' + (el.getAttribute('aria-label') || ''))).map(el => ({id:el.getAttribute('data-plugin-id') || el.getAttribute('data-app-id'),selected:el.getAttribute('aria-checked')==='true'||el.getAttribute('data-state')==='checked'||el.getAttribute('aria-pressed')==='true'||!!el.closest('[data-testid="composer"]')}));
    const users = [...document.querySelectorAll('[data-message-author-role="user"]')];
    const lastUser = text(users.at(-1));
    const markers = (lastUser.match(/req_[a-f0-9]{48}(?:_completion_reminder)?/g) || []).flatMap(m=>[m,m.replace(/_completion_reminder$/,'')]);
    return {host:url.hostname,path:url.pathname,conversation,project,input:visible(input),model:text(model),generating:visible(stop),approval:approvals,plugins,last_markers:markers,user_count:users.length,challenge:!!document.querySelector('iframe[src*="challenges.cloudflare.com"],#challenge-running'),login:!!document.querySelector('[data-testid="login-button"]')};
  });
}
function modelMatches(label, model) {
  const value = label.replace(/^ChatGPT\s*/i,'').replace(/\s+/g,' ').trim().toLowerCase();
  return value === (model === 'web/gpt-6' ? 'gpt-6' : model === 'web/gpt-5.6-sol' ? 'gpt-5.6 sol' : '__unsupported__');
}
function verify(observed, input, {model=true, plugin=true, history=true} = {}) {
  if (observed.host !== 'chatgpt.com') fail('wrong_origin');
  if (observed.challenge) fail('browser_verification_required');
  if (observed.login || !observed.input) fail('login_or_composer_unavailable');
  if (input.conversation_id && observed.conversation !== input.conversation_id) fail('conversation_mismatch');
  if (observed.project !== (input.project_id || '')) fail('project_mismatch');
  if (model && input.model && !modelMatches(observed.model,input.model)) fail('model_mismatch');
  if (plugin && (!observed.plugins.some(p=>p.selected) || new Set(observed.plugins.map(p=>p.id)).size!==1)) fail('plugin_identity_unverified');
  if (plugin && input.plugin_id && !observed.plugins.some(p=>p.selected && p.id===input.plugin_id)) fail('plugin_identity_mismatch');
  if (history && input.expected_last_request_id && !observed.last_markers.includes(input.expected_last_request_id)) fail('page_history_mismatch');
}
async function navigate(page,url) {
  await cdp('Page.navigate',{url},page.session);
  for (let i=0;i<30;i++) { await pause(300); const state=await observe(page); if (state.input && !state.challenge && !state.login) return state; }
  fail('page_not_ready');
}
async function clickUnique(page, selector, labels) {
  const result = await evaluate(page,(selector,labels)=>{
    const elements=[...document.querySelectorAll(selector)].filter(el=>el.getClientRects().length && labels.includes((el.textContent||'').trim()));
    if(elements.length!==1) return false;
    elements[0].click();return true;
  },selector,labels);
  if (!result) fail('control_not_unique');
  await pause(250);
}
async function sendPrompt(page,input) {
  const before=await observe(page);
  verify(before,input);
  if(before.generating || before.approval) fail('page_busy');
  if(before.last_markers.includes(input.request_id)) fail('submission_already_present');
  const inserted=await evaluate(page,(prompt)=>{
    const el=document.querySelector('#prompt-textarea');
    if(!el || !el.isContentEditable) return false;
    el.focus(); document.execCommand('selectAll',false);document.execCommand('insertText',false,prompt);
    el.dispatchEvent(new InputEvent('input',{bubbles:true,inputType:'insertText',data:prompt}));
    return el.innerText.trim()===prompt.trim();
  },input.prompt);
  if(!inserted) fail('composer_insert_failed');
  const sent=await evaluate(page,()=>{
    const buttons=[...document.querySelectorAll('[data-testid="send-button"],button[aria-label="Send prompt"],button[aria-label="发送提示"]')].filter(el=>el.getClientRects().length&&!el.disabled);
    if(buttons.length!==1)return false;buttons[0].click();return true;
  });
  if(!sent)fail('send_control_unavailable');
  for(let i=0;i<30;i++){
    await pause(300);const state=await observe(page);
    if(state.user_count>before.user_count&&state.last_markers.includes(input.request_id)&&state.conversation){
      verify(state,{...input,conversation_id:state.conversation,expected_last_request_id:''},{history:false});
      return {ok:true,received:true,binding:{...page.binding,conversation_id:state.conversation}};
    }
  }
  fail('submission_uncertain');
}
