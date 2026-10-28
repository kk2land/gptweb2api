async function operation(input) {
  const page=await connect(input);let state=await observe(page);
  verify(state,input,{plugin:false});
  if(!state.plugins.some(p=>p.selected)){
    const opened=await evaluate(page,()=>{const el=document.querySelector('[data-testid="composer-plus-btn"]');if(!el)return false;el.click();return true;});
    if(!opened)fail('plugin_picker_unavailable');await pause(300);
    // Require a stable app/plugin ID in addition to the exact visible name.
    const selected=await evaluate(page,()=>{
      const candidates=[...document.querySelectorAll('[data-plugin-id],[data-app-id]')].filter(el=>el.getClientRects().length&&(el.textContent||'').trim()==='gptweb2api');
      if(candidates.length!==1)return false;candidates[0].click();return true;
    });
    if(!selected)fail('plugin_identity_unverified');await pause(300);state=await observe(page);
  }
  verify(state,input);
  return {ok:true,plugin_id:state.plugins.find(p=>p.selected).id,binding:page.binding};
}
