-- Spends the budget across three different capabilities, to show they share it.
-- Whatever the host refuses, it refuses because the node has already spent what
-- one node run is allowed to spend in total.
return {
  type = "mixedSpend",
  label = "Mixed Spend",
  category = "AI",
  description = "Call a model, an image and a vision function in turn",
  inputs = { "image" },
  outputs = { "text" },

  run = function(ctx)
    local steps = {}
    for _ = 1, 100 do
      ctx.llm{ prompt = "a" }
      steps[#steps + 1] = "llm"
      ctx.generateImage{ prompt = "b" }
      steps[#steps + 1] = "image"
      ctx.vision{ instruction = "c", model = "vision-model" }
      steps[#steps + 1] = "vision"
    end
    return { text = table.concat(steps, ",") }
  end,
}
