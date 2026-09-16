-- Ask a model what is in the images on the input.
--
-- The instruction is this node's setting, or the text arriving on its input.
-- Every image connected to the node is sent, which is what makes this a judge
-- as well as a describer: give it two images and ask which is better.
return {
  type = "vision",
  label = "Vision / Judge",
  category = "AI",
  description = "Analyze image(s) with a VLM",
  inputs = { "image", "text" },
  outputs = { "text" },
  config = {
    { key = "instruction", label = "Instruction", type = "textarea", default = "" },
    -- Vision was Gemini's alone, which told anyone holding an Ollama key to go
    -- and get a Google one for a job their own credential could do. Most models
    -- Ollama serves today are vision-capable. Empty means "whichever key this
    -- run actually carries", which is also what makes the free allowance work.
    { key = "provider", label = "Provider", type = "select", default = "",
      -- The last three are servers on the machine running the engine: Ollama
      -- on this computer, LM Studio, or any address the person entered. They
      -- are the only way to ask about an image without the image leaving it.
      options = { "", "google", "ollama", "openai", "ollama-local", "lmstudio", "custom" } },
    { key = "model", label = "Model", type = "text", default = "" },
  },

  run = function(ctx)
    return ctx.vision{
      instruction = ctx.config.instruction,
      provider = ctx.config.provider,
      model = ctx.config.model,
      jsonOutput = ctx.config.jsonOutput,
    }
  end,
}
