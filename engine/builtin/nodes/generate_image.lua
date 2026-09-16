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
    -- Empty means "whichever backend the account has a key for", which is what
    -- makes a workflow portable: the same graph runs on Gemini for somebody who
    -- brought a Google key and on FLUX for somebody who brought a Black Forest
    -- one, without either of them editing it.
    {
      key = "provider",
      label = "Provider",
      type = "select",
      default = "",
      options = { "", "google", "bfl" },
    },
    { key = "model", label = "Model", type = "text", default = "" },
  },

  run = function(ctx)
    return ctx.generateImage{
      prompt = ctx.config.prompt,
      aspectRatio = ctx.config.aspectRatio,
      imageSize = ctx.config.imageSize,
      model = ctx.config.model,
      provider = ctx.config.provider,
    }
  end,
}
