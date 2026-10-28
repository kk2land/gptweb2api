async function operation(input) {
  const page=await connect(input),state=await observe(page);
  verify(state,input,{model:!!input.model,plugin:!!input.model});
  return {ok:true,binding:page.binding,version:checked(await ego.getBrowserVersion()).currentVersion,model:state.model};
}
