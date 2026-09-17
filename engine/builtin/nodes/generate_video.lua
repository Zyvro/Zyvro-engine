-- Text to video.
--
-- The prompt is this node's setting, or the text arriving on its input when the
-- setting is empty — the same arrangement the image node has. An image on the
-- input opens the shot; a second one closes it, and the model interpolates
-- between the two. Those never travel through this file: the graph holds them
-- already, and a video node that copied megabytes of base64 into Lua would pay
-- for it twice.
--
-- What is worth knowing before running one: both backends bill by the second,
-- and a clip takes tens of seconds to render. That is why the settings that
-- decide the price — resolution and duration — are on the node rather than
-- buried in a provider default, and why the engine refuses a combination the
-- backend cannot do instead of sending it and waiting.
return {
  type = "generateVideo",
  label = "Generate Video",
  category = "AI",
  description = "Text -> video, on Veo or FLUX",
  inputs = { "text", "image" },
  outputs = { "video" },
  config = {
    { key = "prompt", label = "Prompt", type = "textarea", default = "" },
    -- Empty means "whichever backend this run has a key for", which is what
    -- makes a workflow portable between somebody who brought a Google key and
    -- somebody who brought a Black Forest Labs one.
    {
      key = "provider",
      label = "Provider",
      type = "select",
      default = "",
      options = { "", "google", "bfl" },
    },
    { key = "model", label = "Model", type = "text", default = "" },
    -- The vocabulary of the price list. Veo has no QHD step and says so rather
    -- than quietly rendering the one below.
    {
      key = "resolution",
      label = "Resolution",
      type = "select",
      default = "",
      options = { "", "hd", "fhd", "qhd", "uhd" },
    },
    -- Zero means the backend's own: auto on FLUX, eight seconds on Veo. FLUX
    -- takes 5 to 20 whole seconds, Veo takes 4, 6 or 8.
    { key = "duration", label = "Seconds", type = "number", default = 0 },
    {
      key = "aspectRatio",
      label = "Aspect ratio",
      type = "select",
      default = "",
      options = { "", "16:9", "9:16", "1:1", "4:3", "3:4", "21:9", "2:1" },
    },
    -- The cheap pass: a third of the price, HD only, for finding out whether
    -- the prompt is the one you meant before paying for the real thing. FLUX
    -- has it; Veo does not.
    { key = "draft", label = "Draft (FLUX, cheaper)", type = "boolean", default = false },
  },

  run = function(ctx)
    return ctx.generateVideo{
      prompt = ctx.config.prompt,
      provider = ctx.config.provider,
      model = ctx.config.model,
      resolution = ctx.config.resolution,
      duration = ctx.config.duration,
      aspectRatio = ctx.config.aspectRatio,
      draft = ctx.config.draft,
    }
  end,
}
