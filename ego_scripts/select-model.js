async function operation(input) {
  const page=await connect(input);let state=await observe(page);
  verify(state,input,{model:false,plugin:false});
  if(!modelMatches(state.model,input.model)){
    const opened=await evaluate(page,()=>{const el=document.querySelector('[data-testid="model-switcher-dropdown-button"]');if(!el)return false;el.click();return true;});
    if(!opened)fail('model_picker_unavailable');await pause(300);
    const labels=input.model==='web/gpt-6'?['GPT-6']:input.model==='web/gpt-5.6-sol'?['GPT-5.6 Sol']:[];
    await clickUnique(page,'[role="menuitem"],[role="menuitemradio"],[role="option"]',labels);
    state=await observe(page);
  }
  if(!modelMatches(state.model,input.model))fail('requested_model_unavailable');
  return {ok:true,model:input.model,binding:page.binding};
}
