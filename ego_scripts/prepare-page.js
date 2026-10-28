async function operation(input) {
  const b={...input.binding};
  if(!b.space_id||!b.profile_id)fail('space_binding_missing');
  ego.useTaskSpace(b.space_id);
  const space=checked(await ego.listTaskSpaces()).taskSpaces.find(s=>s.id===b.space_id);
  if(!space||space.ownership!=='agent'||space.profileId!==b.profile_id)fail('space_binding_changed');
  if(!b.page_id)b.page_id=checked(await ego.createTab('https://chatgpt.com/')).targetId;
  else if(!checked(await ego.listTabs()).tabs.some(t=>t.targetId===b.page_id))fail('page_binding_changed');
  return {ok:true,binding:b};
}
