-- Mirror the image on the input. "h" flips left to right, "v" top to bottom.
-- No model call.
return {
  type = "flipImage",
  label = "Flip Image",
  category = "Utility",
  description = "Mirror an image horizontally or vertically",
  inputs = { "image" },
  outputs = { "image" },
  config = {
    { key = "axis", label = "Axis", type = "select", options = { "h", "v" }, default = "h" },
  },

  run = function(ctx)
    return ctx.flipImage{ axis = ctx.config.axis }
  end,
}
