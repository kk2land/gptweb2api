async function operation(input) {
  const page=await connect(input),state=await observe(page);
  verify(state,{...input,conversation_id:page.binding.conversation_id,expected_last_request_id:input.request_id});
  // The reminder has a separate transport marker so submit cannot treat the
  // original user message as proof that this new reminder was received.
  const marker=input.request_id+'_completion_reminder';
  return sendPrompt(page,{...input,conversation_id:page.binding.conversation_id,expected_last_request_id:input.request_id,request_id:marker,prompt:input.prompt+'\nReminder marker: '+marker});
}
