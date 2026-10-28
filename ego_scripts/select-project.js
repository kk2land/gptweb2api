async function operation(input) {
  const page=await connect(input),state=await observe(page);
  // Navigation happened once in create-conversation. Validate actual ownership.
  verify(state,input,{model:false,plugin:false});
  return {ok:true,binding:page.binding};
}
