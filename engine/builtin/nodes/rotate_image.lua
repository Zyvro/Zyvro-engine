-- Turn the image on the input by a multiple of 90 degrees. No model call.
return {
  type = "rotateImage",
  label = "Rotate Image",
  category = "Utility",
  description = "Rotate an image by 90-degree steps",
  inputs = { "image" },
  outputs = { "image" },
  config = {
    { key = "degrees", label = "Degrees", type = "number", default = 90 },
  },

  run = function(ctx)
    return ctx.rotateImage{ degrees = ctx.config.degrees }
  end,
}
