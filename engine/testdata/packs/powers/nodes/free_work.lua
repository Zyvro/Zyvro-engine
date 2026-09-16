-- Turns and mirrors the image on its input many times over. None of that
-- reaches a provider, so none of it comes out of the model budget: a node that
-- rotates six images should still be able to call a model afterwards.
return {
  type = "freeWork",
  label = "Free Work",
  category = "Utility",
  description = "Rotate and flip repeatedly, then call a model once",
  inputs = { "image" },
  outputs = { "text" },

  run = function(ctx)
    for _ = 1, 20 do
      ctx.rotateImage{ degrees = 90 }
      ctx.flipImage{ axis = "v" }
    end
    return { text = ctx.llm{ prompt = "and now one model call" } }
  end,
}
