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
  },

  run = function(ctx)
    return ctx.vision{
      instruction = ctx.config.instruction,
      model = ctx.config.model,
      jsonOutput = ctx.config.jsonOutput,
    }
  end,
}
