async function operation(input) {
  const page=await connect(input),state=await observe(page);
  verify(state,{...input,conversation_id:page.binding.conversation_id,expected_last_request_id:input.request_id});
  return {ok:true,state:state.approval?'waiting_approval':state.generating?'generating':'idle',binding:page.binding};
}
