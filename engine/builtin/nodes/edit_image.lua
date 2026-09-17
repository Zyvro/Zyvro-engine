-- Edit the image on the input with a prompt.
--
-- The prompt is this node's setting, or the text arriving on its input. The
-- image is whatever is connected: a node edits what the graph gave it.
return {
  type = "editImage",
  label = "Edit Image",
  category = "AI",
  description = "Edit an image with a prompt",
  inputs = { "image", "text" },
  outputs = { "image" },
  config = {
    { key = "prompt", label = "Prompt", type = "textarea", default = "" },
    -- Editing is not Gemini's alone. An image and a prompt in, an image out,
    -- is what FLUX does and what a diffusion server on this machine does;
    -- custom-image is that server, and the image never leaves the computer.
    {
      key = "provider",
      label = "Provider",
      type = "select",
      default = "",
      options = { "", "google", "bfl", "custom-image" },
    },
    { key = "model", label = "Model", type = "text", default = "" },
  },

  run = function(ctx)
    return ctx.editImage{
      prompt = ctx.config.prompt,
      provider = ctx.config.provider,
      model = ctx.config.model,
    }
  end,
}
