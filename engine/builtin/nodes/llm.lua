-- Text completion.
--
-- ctx.complete is the llm capability: the host holds the provider, the
-- credential and the routing, and this node hands it settings. An empty prompt
-- falls back to the text arriving on the input, and jsonOutput asks for the
-- JSON object inside the answer instead of the answer itself; both happen on
-- the host side, which is why the node returns the host's result rather than
-- building one of its own.
return {
  type = "llm",
  label = "LLM",
  category = "AI",
  description = "Text completion via Ollama, Claude or OpenAI",
  inputs = { "text" },
  outputs = { "text" },
  config = {
    { key = "system", label = "System", type = "textarea", default = "" },
    { key = "prompt", label = "Prompt", type = "textarea", default = "" },
    { key = "temperature", label = "Temperature", type = "number", default = 0.7 },
    { key = "maxTokens", label = "Max tokens", type = "number", default = 2000 },
    { key = "provider", label = "Provider", type = "text", default = "" },
    { key = "model", label = "Model", type = "text", default = "" },
  },

  run = function(ctx)
    return ctx.complete{
      system = ctx.config.system,
      prompt = ctx.config.prompt,
      temperature = ctx.config.temperature,
      maxTokens = ctx.config.maxTokens,
      provider = ctx.config.provider,
      model = ctx.config.model,
      jsonOutput = ctx.config.jsonOutput,
    }
  end,
}
