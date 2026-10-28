async function operation(input) {
  // Dot tasks use a Space quota but never navigate a webpage or select a chat.
  if(!globalThis.ego)fail('ego_bindings_missing');
  let b=input.binding||{};
  const spaces=checked(await ego.listTaskSpaces()).taskSpaces;
  if(!b.space_id){
    if(spaces.some(s=>s.name===input.space_name))fail('space_requires_reconciliation');
    const created=checked(await ego.createTaskSpace(input.space_name,input.profile_id||undefined));
    const space=checked(await ego.listTaskSpaces()).taskSpaces.find(s=>s.id===created.id);
    if(!space?.profileId)fail('profile_unverified');b={space_id:created.id,profile_id:space.profileId,page_id:'',conversation_id:''};
  }else{
    const space=spaces.find(s=>s.id===b.space_id);
    if(!space||space.ownership!=='agent'||space.profileId!==b.profile_id)fail('space_binding_changed');
  }
  return {ok:true,binding:b};
}
