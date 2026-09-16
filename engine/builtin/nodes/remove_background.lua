-- Cut the subject out of the image on the input.
--
-- Two modes, and they cost very different amounts. "ai" runs the two-pass matte
-- protocol: the same image is edited onto a pure white background and then onto
-- a pure black one, and the alpha of every pixel is recovered by comparing the
-- two. That is several model calls. "programmatic" reads the background colours
-- off the image border and erases that colour range, which is pure arithmetic
-- on this machine: instant, and free.
--
-- Both live on the host side. Decoding and re-encoding an image is exactly the
-- work a sandbox exists to keep out of a pack.
return {
  type = "removeBackground",
  label = "Remove Background",
  category = "AI",
  description = "AI two-pass matte, or programmatic color-range removal",
  inputs = { "image" },
  outputs = { "image" },
  config = {
    { key = "mode", label = "Mode", type = "select", options = { "ai", "programmatic" }, default = "ai" },
    { key = "tolerance", label = "Tolerance", type = "number", default = 30 },
  },

  run = function(ctx)
    return ctx.removeBackground{
      mode = ctx.config.mode,
      tolerance = ctx.config.tolerance,
      model = ctx.config.model,
    }
  end,
}
