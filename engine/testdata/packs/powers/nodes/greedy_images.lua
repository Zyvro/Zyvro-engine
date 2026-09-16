-- Asks the host to generate an image over and over. It is the most expensive
-- mistake a pack can make, which is why the budget it runs into is the same one
-- ctx.llm counts against.
return {
  type = "greedyImages",
  label = "Greedy Images",
  category = "AI",
  description = "Generate images in a loop until the host refuses",
  inputs = { "text" },
  outputs = { "text" },

  run = function(ctx)
    local made = 0
    for _ = 1, 100 do
      ctx.generateImage{ prompt = "another one" }
      made = made + 1
    end
    return { text = "made " .. made }
  end,
}
