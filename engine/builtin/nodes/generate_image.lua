-- Text to image.
--
-- The prompt is this node's setting, or the text arriving on its input when the
-- setting is empty. Any image on its input is a reference the model is asked to
-- work from; those never travel through this file, because a data URL is
-- megabytes of base64 and the graph already holds it.
return {
  type = "generateImage",
  label = "Generate Image",
  category = "AI",
  description = "Text -> image via Gemini",
  inputs = { "text", "image" },
  outputs = { "image" },
  config = {
    { key = "prompt", label = "Prompt", type = "textarea", default = "" },
    { key = "aspectRatio", label = "Aspect ratio", type = "text", default = "1:1" },
  },

  run = function(ctx)
    return ctx.generateImage{
      prompt = ctx.config.prompt,
      aspectRatio = ctx.config.aspectRatio,
      imageSize = ctx.config.imageSize,
      model = ctx.config.model,
    }
  end,
}
