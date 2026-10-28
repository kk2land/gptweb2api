async function operation(input) {
  initCDP();
  let b=input.binding||{};
  if (!b.space_id) {
    const spaces=checked(await ego.listTaskSpaces()).taskSpaces;
    if (spaces.some(s=>s.name===input.space_name)) fail('space_requires_reconciliation');
    const created=checked(await ego.createTaskSpace(input.space_name,input.profile_id||undefined));
    const space=checked(await ego.listTaskSpaces()).taskSpaces.find(s=>s.id===created.id);
    if(!space?.profileId)fail('profile_unverified');
    b={space_id:created.id,profile_id:space.profileId,page_id:'',conversation_id:''};
  }
  ego.useTaskSpace(b.space_id);
  if(!b.page_id)b.page_id=checked(await ego.createTab('https://chatgpt.com/')).targetId;
  const page=await connect({...input,binding:b});
  const project=input.project_id?`/g/${encodeURIComponent(input.project_id)}`:'';
  const url=input.conversation_id?`https://chatgpt.com${project}/c/${encodeURIComponent(input.conversation_id)}`:input.project_id?`https://chatgpt.com${project}/project`:'https://chatgpt.com/';
  const state=await navigate(page,url);
  verify(state,input,{model:false,plugin:false});
  return {ok:true,binding:{...b,conversation_id:state.conversation}};
}
