async function operation(input) {
  // Dot tasks use a Space quota but never navigate a webpage or select a chat.
  return {ok:true,binding:await ensureTaskSpace(input)};
}
