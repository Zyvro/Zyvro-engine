-- Reads one file from the project folder and hands it on as text.
return {
  type = "readText",
  label = "Read Text",
  category = "Input",
  description = "Read a text file from the project folder",
  inputs = {},
  outputs = { "text" },
  config = {
    { key = "path", label = "Path", type = "text", default = "" },
  },

  run = function(ctx)
    local text = ctx.readFile(ctx.config.path)
    return { text = text }
  end,
}
