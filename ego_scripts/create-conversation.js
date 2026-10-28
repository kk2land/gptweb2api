async function operation(input) {
  initCDP();
  const b=await ensureTaskSpace(input);
  ego.useTaskSpace(b.space_id);
  if(!b.page_id)b.page_id=checked(await ego.createTab('https://chatgpt.com/')).targetId;
  const page=await connect({...input,binding:b});
  const project=input.project_id?`/g/${encodeURIComponent(input.project_id)}`:'';
  const url=input.conversation_id?`https://chatgpt.com${project}/c/${encodeURIComponent(input.conversation_id)}`:input.project_id?`https://chatgpt.com${project}/project`:'https://chatgpt.com/';
  const state=await navigate(page,url);
  verify(state,input,{model:false,plugin:false});
  return {ok:true,binding:{...b,conversation_id:state.conversation}};
}
