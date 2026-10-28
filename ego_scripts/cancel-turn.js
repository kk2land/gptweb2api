async function operation(input) {
  const page=await connect(input);let state=await observe(page);
  if(state.host!=='chatgpt.com')fail('wrong_origin');
  if(input.binding.conversation_id&&state.conversation!==input.binding.conversation_id)fail('conversation_mismatch');
  if(state.generating){
    if(!input.binding.conversation_id && (!input.request_id || !state.last_markers.includes(input.request_id)))fail('turn_identity_unverified');
    const stopped=await evaluate(page,()=>{const el=document.querySelector('[data-testid="stop-button"],button[aria-label="Stop generating"],button[aria-label="停止生成"]');if(!el)return false;el.click();return true;});
    if(!stopped)fail('stop_unconfirmed');
  }
  for(let i=0;i<20;i++){state=await observe(page);if(!state.generating&&!state.approval)return {ok:true,state:'idle',binding:page.binding};await pause(300);}
  fail('stop_unconfirmed');
}
