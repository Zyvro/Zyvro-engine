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
  },

  run = function(ctx)
    return ctx.editImage{
      prompt = ctx.config.prompt,
      model = ctx.config.model,
    }
  end,
}
