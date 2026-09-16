-- The simplest possible node: no model, no capabilities, pure computation.
-- It exists mostly as a worked example, and because counting words is the kind
-- of thing nobody wants to spend a model call on.
return {
  type = "wordCount",
  label = "Word Count",
  category = "Utility",
  description = "Count words, characters and lines in the incoming text",
  inputs = { "text" },
  outputs = { "json" },
  config = {},

  run = function(ctx)
    local text = ctx.input.text or ""

    local words = 0
    for _ in string.gmatch(text, "%S+") do
      words = words + 1
    end

    -- A trailing newline should not count as an extra line, which is why this
    -- counts separators and adds one rather than counting newlines.
    local lines = 1
    for _ in string.gmatch(text, "\n") do
      lines = lines + 1
    end
    if text == "" then lines = 0 end

    return {
      data = {
        words = words,
        characters = string.len(text),
        lines = lines,
      },
    }
  end,
}
